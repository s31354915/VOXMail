package mailsync

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/voxmail/voxmail/internal/diagnostics"
	"github.com/voxmail/voxmail/internal/maintenance"
	"github.com/voxmail/voxmail/internal/operation"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	remoteimap "github.com/voxmail/voxmail/internal/imap"
	"github.com/voxmail/voxmail/internal/lifecycle"
	"github.com/voxmail/voxmail/internal/mailconfig"
	"github.com/voxmail/voxmail/internal/mailindex"
	"github.com/voxmail/voxmail/internal/netguard"
	"github.com/voxmail/voxmail/internal/secret"
	"github.com/voxmail/voxmail/internal/store"
)

var ErrSyncBusy = errors.New("account synchronization is already running")
var ErrSyncCapacity = errors.New("manual synchronization capacity is full")

const maxManualSyncJobs = 8

// Defaults for the mbsync hard caps. Five minutes bounds an incremental pass;
// initial, reconciliation, and manual runs may legitimately need far longer,
// but every kind is still bounded so a stuck child cannot block forever.
const (
	defaultSyncTimeout        = 5 * time.Minute
	defaultInitialSyncTimeout = 30 * time.Minute
)

// runTimeout returns the hard execution cap for one mbsync invocation of the
// given kind. A run that exceeds it is killed and recorded as failed rather
// than left open indefinitely.
// defaultMaxConcurrentSyncs is the scheduled sweep's concurrency bound. Two
// keeps one slow provider from holding later accounts hostage while leaving
// headroom for the single SQLite connection the store shares.
const defaultMaxConcurrentSyncs = 2

// syncConcurrencyLimit returns the configured scheduled-sync concurrency,
// floored at 1 so a misconfigured value cannot deadlock the sweep.
func (s *Service) syncConcurrencyLimit() int {
	if s.MaxConcurrentSyncs > 0 {
		return s.MaxConcurrentSyncs
	}
	return defaultMaxConcurrentSyncs
}

func (s *Service) runTimeout(kind string) time.Duration {
	if kind == "initial" || kind == "reconciliation" || kind == "manual" {
		if s.InitialSyncTimeout > 0 {
			return s.InitialSyncTimeout
		}
		return defaultInitialSyncTimeout
	}
	if s.SyncTimeout > 0 {
		return s.SyncTimeout
	}
	return defaultSyncTimeout
}

type Service struct {
	manualWG    sync.WaitGroup
	manualSlots chan struct{}
	Store       SyncStore
	Root        string
	// QuarantineRoot optionally receives stale local messages before their
	// index rows are removed. An empty value preserves direct removal.
	QuarantineRoot string
	Runner         Runner
	Log            *slog.Logger
	Index          *mailindex.Indexer
	Secrets        *secret.Box
	// SyncTimeout hard-caps one incremental mbsync invocation so a hung
	// tunnel or child can never block future runs. Initial, reconciliation,
	// and manual runs legitimately walk whole mailboxes, so they use
	// InitialSyncTimeout instead. Zero falls back to the package defaults.
	SyncTimeout        time.Duration
	InitialSyncTimeout time.Duration
	// MaxConcurrentSyncs bounds how many due accounts SyncDue runs at once.
	// Zero or a negative value falls back to the package default of 2; values
	// below 1 never make sense, so they keep the default too.
	MaxConcurrentSyncs int
	// IdleEnabled turns on per-account IMAP IDLE watchers so new INBOX mail
	// triggers a targeted sync without waiting for the next scheduled sweep.
	// Polling through SyncDue continues either way; the watchers only shorten
	// the gap, and any watcher failure falls back to polling. Off keeps
	// sweep-only behavior.
	IdleEnabled bool
	// EndpointResolver is required in production before mbsync is launched. It
	// must return the exact numeric address that was policy-checked and dialed;
	// the generated mbsync tunnel then uses that address without re-resolving
	// the user-controlled hostname.
	EndpointResolver func(context.Context, string, int) (string, error)
	mu               sync.Mutex
	busy             map[string]bool
	deleting         map[string]bool
	activeCancel     map[string]context.CancelFunc
	activeKind       map[string]string
	activeDone       map[string]chan struct{}
	last             map[string]time.Time
	running          atomic.Bool
	now              func() time.Time
}

// SyncStore is the narrow persistence boundary used by synchronization. It
// keeps reconciliation and scheduling independent from the SQLite handle and
// makes failure-path tests able to inject only the operations they exercise.
type SyncStore interface {
	ListAccounts(context.Context, string) ([]store.Account, error)
	ListAllAccounts(context.Context) ([]store.Account, error)
	LatestSyncRun(context.Context, string, bool) (store.SyncRun, error)
	LatestFullSyncRun(context.Context, string) (store.SyncRun, error)
	BeginSyncRun(context.Context, string, string) (int64, error)
	FinishSyncRun(context.Context, int64, bool, bool, error) error
	ListFolderRoles(context.Context, string, string) ([]store.FolderRole, error)
	BindSyncFolders(context.Context, string, string, int) error
	RecordFolderDiscovery(context.Context, string, string, int, []string) error
	PhysicalFoldersForRemote(context.Context, string, string, string) ([]string, error)
	UpsertRemoteFolder(context.Context, string, string, string, string) error
	PruneRemoteFolders(context.Context, string, []string) error
	UpdateMailIdentity(context.Context, string, string, string, uint32, uint32, []string, bool) error
	UpdateMailIdentityByUID(context.Context, string, string, uint32, uint32, []string, bool) error
	QueuedMutationWatermark(context.Context, string) (int64, error)
	QueuedReadMutations(context.Context, string, int64) ([]store.QueuedReadMutation, error)
	RecentReadMutations(context.Context, string, string) ([]store.QueuedReadMutation, error)
	LatestReadForIdentity(context.Context, string, []string, uint32, uint32) (store.QueuedReadMutation, error)
	FinishQueuedRead(context.Context, store.QueuedReadMutation, error) error
	LockMailReadState() func()
	ReadIdentityCopies(context.Context, string, []string, uint32, uint32) ([]store.MailSummary, error)
	MarkConfirmedReadAtPath(context.Context, store.MailSummary, bool, string) error
	RemoteFolderForLocal(context.Context, string, string, string) (string, error)
	MarkQueuedMutationsSynchronized(context.Context, string, int64, []string) error
	Audit(context.Context, string, string, string) error
	ListIndexedMail(context.Context, string) ([]store.IndexedMail, error)
	UpdateIndexedMailUID(context.Context, string, int64, uint32, uint32) error
	DeleteIndexedMail(context.Context, string, int64) error
}

const syncRetryBackoff = 5 * time.Minute

// readReconcileGrace bounds how long reconciliation keeps protecting a local
// read change after it was handed to mbsync, even though the provider's own
// flags may not reflect it yet.
const readReconcileGrace = 30 * time.Minute

// reconciliationConnectionBudget bounds one authenticated reconciliation
// connection walking every remote folder. Each IMAP operation keeps its own
// per-command deadline, so this outer budget only stops a silent provider
// from holding the connection forever; a slow earlier folder can no longer
// spend the clock of the folders that follow it.
const reconciliationConnectionBudget = 10 * time.Minute

func (s *Service) Run(ctx context.Context) error {
	if s.Store == nil || s.Root == "" {
		return fmt.Errorf("sync store and root are required")
	}
	s.ensureState()
	s.running.Store(true)
	defer s.running.Store(false)
	if s.IdleEnabled {
		_ = s.startIdleWatch(ctx)
	}
	_ = s.SyncAll(ctx)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			_ = s.SyncDue(ctx)
		}
	}
}

// Healthy reports whether the long-lived synchronization worker is running.
// Account-level failures remain visible through durable sync_runs records.
func (s *Service) Healthy() bool {
	return s != nil && s.running.Load()
}

// SyncRunning reports live ownership of an account's synchronization work.
// An unfinished database record alone does not prove that a worker is alive.
func (s *Service) SyncRunning(accountID string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busy[accountID]
}

func (s *Service) SyncAll(ctx context.Context) error {
	s.ensureState()
	accounts, err := s.Store.ListAllAccounts(ctx)
	if err != nil {
		return err
	}
	var syncErr error
	for _, account := range accounts {
		if err := s.syncAccount(ctx, account, s.nextKind(ctx, account)); err != nil {
			if s.Log != nil {
				s.Log.Error("mail sync failed", "account", account.ID, "error", err)
			}
			syncErr = errors.Join(syncErr, fmt.Errorf("account %s: %w", account.ID, err))
		}
	}
	return syncErr
}

func (s *Service) SyncDue(ctx context.Context) error {
	s.ensureState()
	accounts, err := s.Store.ListAllAccounts(ctx)
	if err != nil {
		return err
	}
	now := s.currentTime()
	due := make([]store.Account, 0, len(accounts))
	for _, account := range accounts {
		interval := time.Duration(account.SyncIntervalMinutes) * time.Minute
		if interval <= 0 {
			interval = 5 * time.Minute
		}
		last := time.Time{}
		if run, err := s.Store.LatestSyncRun(ctx, account.ID, true); err == nil {
			if parsed, parseErr := time.Parse(time.RFC3339Nano, run.FinishedAt); parseErr == nil {
				last = parsed
			}
		} else {
			s.mu.Lock()
			last = s.last[account.ID]
			s.mu.Unlock()
		}
		if run, err := s.Store.LatestSyncRun(ctx, account.ID, false); err == nil && failedRunInBackoff(run, now) {
			continue
		}
		if last.IsZero() || now.Sub(last) >= interval {
			due = append(due, account)
		}
	}
	// Due accounts used to sync strictly sequentially, so one slow provider
	// held every other account's schedule hostage. Run them concurrently with
	// a small bound; per-account ownership is still enforced by the busy map
	// inside syncAccount.
	sem := make(chan struct{}, s.syncConcurrencyLimit())
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var syncErr error
	for _, account := range due {
		wg.Add(1)
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Done()
			wg.Wait()
			return errors.Join(syncErr, ctx.Err())
		}
		go func(account store.Account) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := s.syncAccount(ctx, account, s.nextKind(ctx, account)); err != nil {
				if errors.Is(err, store.ErrAccountDeleting) {
					// The account is being removed; skip it quietly instead
					// of failing the whole sweep.
					return
				}
				if s.Log != nil {
					s.Log.Error("mail sync failed", "account", account.ID, "error", err)
				}
				errMu.Lock()
				syncErr = errors.Join(syncErr, fmt.Errorf("account %s: %w", account.ID, err))
				errMu.Unlock()
			}
		}(account)
	}
	wg.Wait()
	return syncErr
}

func failedRunInBackoff(run store.SyncRun, now time.Time) bool {
	if run.Success {
		return false
	}
	// An unparseable FinishedAt used to defeat the backoff entirely, so a
	// failing account retried on every scheduler tick. Fall back to
	// StartedAt before giving up on the timestamp.
	parsed, err := time.Parse(time.RFC3339Nano, run.FinishedAt)
	if err != nil {
		parsed, err = time.Parse(time.RFC3339Nano, run.StartedAt)
	}
	if err != nil {
		return false
	}
	return now.Before(parsed.Add(syncRetryBackoff))
}

// RefreshAccount performs an on-demand synchronization for one account after
// verifying ownership.  It uses the same guarded path as scheduled syncs, so
// a phone-triggered refresh cannot overlap the periodic worker.
func (s *Service) RefreshAccount(ctx context.Context, userID, accountID string) error {
	if s.Store == nil || strings.TrimSpace(userID) == "" || strings.TrimSpace(accountID) == "" {
		return fmt.Errorf("user, account, and store are required")
	}
	accounts, err := s.Store.ListAccounts(ctx, userID)
	if err != nil {
		return err
	}
	for _, account := range accounts {
		if account.ID == accountID {
			return s.syncAccount(ctx, account, "manual")
		}
	}
	return fmt.Errorf("account not found")
}

// StartRefreshAccount admits a durable run before returning its ID. The
// request context owns validation/waiting; workerCtx owns the admitted job.
// Callers must close admission before WaitManualSyncs, as Server.Close does.
func (s *Service) StartRefreshAccount(requestCtx, workerCtx context.Context, userID, accountID string) (int64, error) {
	if s.Store == nil || userID == "" || accountID == "" {
		return 0, fmt.Errorf("user, account, and store are required")
	}
	accounts, err := s.Store.ListAccounts(requestCtx, userID)
	if err != nil {
		return 0, err
	}
	var owned *store.Account
	for i := range accounts {
		if accounts[i].ID == accountID {
			owned = &accounts[i]
			break
		}
	}
	if owned == nil {
		return 0, fmt.Errorf("account not found")
	}
	if workerCtx == nil {
		return 0, fmt.Errorf("manual sync worker context is required")
	}
	s.ensureState()
	select {
	case s.manualSlots <- struct{}{}:
	default:
		return 0, ErrSyncCapacity
	}
	type admission struct {
		id  int64
		err error
	}
	started := make(chan admission, 1)
	s.manualWG.Add(1)
	go func() {
		defer s.manualWG.Done()
		defer func() { <-s.manualSlots }()
		err := s.syncAccountStarted(workerCtx, *owned, "manual", func(id int64, err error) { started <- admission{id, err} })
		if err != nil && s.Log != nil {
			s.Log.Warn("manual synchronization failed", "account", accountID, "category", operation.Classify(err))
		}
	}()
	select {
	case result := <-started:
		return result.id, result.err
	case <-requestCtx.Done():
		return 0, requestCtx.Err()
	}
}

func (s *Service) WaitManualSyncs() { s.manualWG.Wait() }

func (s *Service) ensureState() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.manualSlots == nil {
		s.manualSlots = make(chan struct{}, maxManualSyncJobs)
	}
	if s.busy == nil {
		s.busy = make(map[string]bool)
	}
	if s.last == nil {
		s.last = make(map[string]time.Time)
	}
	if s.deleting == nil {
		s.deleting = make(map[string]bool)
	}
	if s.activeCancel == nil {
		s.activeCancel = make(map[string]context.CancelFunc)
	}
	if s.activeKind == nil {
		s.activeKind = make(map[string]string)
	}
	if s.activeDone == nil {
		s.activeDone = make(map[string]chan struct{})
	}
}

// CancelManualSync requests cancellation only for an explicitly requested
// account refresh. Scheduled work remains owned by the background worker.
func (s *Service) CancelManualSync(ctx context.Context, accountID string) (bool, error) {
	if s == nil || strings.TrimSpace(accountID) == "" {
		return false, fmt.Errorf("account identity is required")
	}
	if ctx != nil && ctx.Err() != nil {
		return false, ctx.Err()
	}
	s.ensureState()
	s.mu.Lock()
	cancel := s.activeCancel[accountID]
	manual := s.activeKind[accountID] == "manual"
	s.mu.Unlock()
	if !manual || cancel == nil {
		return false, nil
	}
	cancel()
	return true, nil
}

// StopAccount prevents new synchronization, cancels the active run, and
// waits until its worker has released all account resources. The store's
// deleting flag is set by the caller before this method, so a late scheduler
// iteration cannot recreate work for the account.
func (s *Service) StopAccount(ctx context.Context, accountID string) error {
	if s == nil || strings.TrimSpace(accountID) == "" {
		return fmt.Errorf("account identity is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.ensureState()
	s.mu.Lock()
	s.deleting[accountID] = true
	cancel := s.activeCancel[accountID]
	done := s.activeDone[accountID]
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (s *Service) nextKind(ctx context.Context, account store.Account) string {
	run, err := s.Store.LatestSyncRun(ctx, account.ID, true)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "initial"
		}
		// A transient store failure must not downgrade a mature account to
		// an initial sync: the initial cutoff prune would then apply to old
		// mail again. Fall back to the safest ordinary kind instead.
		return "incremental"
	}
	if run.Kind == "initial" {
		return "incremental"
	}
	full, err := s.Store.LatestFullSyncRun(ctx, account.ID)
	if err != nil {
		return "reconciliation"
	}
	interval := time.Duration(account.ReconciliationIntervalMinutes) * time.Minute
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	if parsed, err := time.Parse(time.RFC3339Nano, full.FinishedAt); err == nil && s.currentTime().Sub(parsed) >= interval {
		return "reconciliation"
	}
	return "incremental"
}

func (s *Service) currentTime() time.Time {
	if s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) syncAccount(ctx context.Context, account store.Account, kind string) error {
	return s.syncAccountStarted(ctx, account, kind, nil)
}

func (s *Service) syncAccountStarted(ctx context.Context, account store.Account, kind string, started func(int64, error)) (retErr error) {
	reported := false
	defer func() {
		if started != nil && !reported {
			started(0, retErr)
		}
	}()
	finishTrace := diagnostics.Start(ctx, "mailsync", kind, "account_id", account.ID)
	defer func() { finishTrace(retErr) }()
	s.ensureState()
	s.mu.Lock()
	if s.deleting[account.ID] {
		s.mu.Unlock()
		return store.ErrAccountDeleting
	}
	if s.busy[account.ID] {
		s.mu.Unlock()
		if started != nil {
			return ErrSyncBusy
		}
		return nil
	}
	s.busy[account.ID] = true
	accountCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	s.activeCancel[account.ID] = cancel
	s.activeKind[account.ID] = kind
	s.activeDone[account.ID] = done
	s.mu.Unlock()
	ctx = accountCtx
	defer func() {
		cancel()
		s.mu.Lock()
		delete(s.busy, account.ID)
		delete(s.activeCancel, account.ID)
		delete(s.activeKind, account.ID)
		delete(s.activeDone, account.ID)
		close(done)
		s.mu.Unlock()
	}()
	previous, previousErr := s.Store.LatestSyncRun(ctx, account.ID, false)
	runID, err := s.Store.BeginSyncRun(ctx, account.ID, kind)
	if err != nil {
		return err
	}
	changed := false
	defer func() {
		cleanupCtx, cleanupCancel := lifecycle.CleanupContext(ctx)
		defer cleanupCancel()
		if finishErr := s.Store.FinishSyncRun(cleanupCtx, runID, retErr == nil, changed, retErr); finishErr != nil && retErr == nil {
			retErr = finishErr
		}
	}()
	if started != nil {
		reported = true
		started(runID, nil)
	}
	root := filepath.Join(s.Root, "mail", account.ID)
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	// Recover complete files on every failed exit, including cancellation after
	// the download succeeded but before/during indexing. Keep account ownership
	// until recovery finishes so a new sync cannot race this scan.
	defer func() {
		if retErr == nil || s.Index == nil {
			return
		}
		recoveryCtx, recoveryCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
		defer recoveryCancel()
		if indexErr := s.Index.IndexAvailable(recoveryCtx, account.ID, root); indexErr != nil {
			retErr = errors.Join(retErr, operation.Wrap("mailindex", "partial_sync", operation.MailIndex, indexErr))
		}
	}()
	// A hard process/container stop cannot execute deferred recovery. Import its
	// completed Maildir files before attempting another network connection.
	if s.Index != nil && previousErr == nil && !previous.Success {
		if err := s.Index.IndexAvailable(ctx, account.ID, root); err != nil {
			return operation.Wrap("mailindex", "recover_sync", operation.MailIndex, err)
		}
	}
	if s.EndpointResolver == nil {
		return fmt.Errorf("IMAP endpoint resolver is required")
	}
	port := account.IMAPPort
	if port == 0 {
		port = 993
	}
	imapAddress, err := s.EndpointResolver(ctx, account.IMAPHost, port)
	if err != nil {
		return operation.Wrap("mailsync", "endpoint", operation.EndpointPolicy, err)
	}
	var folderMap map[string]string
	if account.FolderMap != "" {
		_ = json.Unmarshal([]byte(account.FolderMap), &folderMap)
	}
	remoteFolders, err := s.discoverSyncFolders(ctx, account, imapAddress)
	if err != nil {
		return err
	}
	if remoteFolders != nil {
		if err := s.Store.RecordFolderDiscovery(ctx, account.UserID, account.ID, account.ConfigVersion, remoteFolders); err != nil {
			return fmt.Errorf("retain sync discovery: %w", err)
		}
	}
	syncAccount := mailconfig.Account{ID: account.ID, IMAPHost: account.IMAPHost, IMAPAddress: imapAddress, IMAPPort: account.IMAPPort, IMAPSecurity: account.IMAPSecurity, IMAPUser: account.IMAPUser, MaildirRoot: root, FolderMap: folderMap, RemoteFolders: remoteFolders}
	configText, err := mailconfig.Generate(syncAccount)
	if err != nil {
		return err
	}
	configDir := filepath.Join(s.Root, "config", "mbsync")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return err
	}
	configPath := filepath.Join(configDir, account.ID+".conf")
	tmp, releaseTmp, err := maintenance.CreateTemp(configDir, ".conf-")
	if err != nil {
		return err
	}
	defer releaseTmp()
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(configText); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, configPath); err != nil {
		return err
	}
	channels := mailconfig.ChannelNames(syncAccount)
	if err := s.Store.BindSyncFolders(ctx, account.UserID, account.ID, account.ConfigVersion); err != nil {
		return fmt.Errorf("retain sync folder identities: %w", err)
	}
	mutationWatermark, err := s.Store.QueuedMutationWatermark(ctx, account.ID)
	if err != nil {
		return fmt.Errorf("snapshot queued mail mutations: %w", err)
	}
	// Hard-cap the child process: a tunnel that never returns EOF previously
	// wedged the whole run, keeping sync_runs open and starving the outbound
	// alert pipeline of a completed sync.
	runCtx, runCancel := context.WithTimeout(ctx, s.runTimeout(kind))
	defer runCancel()
	result, err := s.Runner.SyncChannels(runCtx, configPath, channels...)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	changed = result.Changed
	// Retry retained folders before indexing/reconciliation can refresh flags.
	// Import downloaded mail even when a retry fails, then report that failure.
	retainedReadErr := s.retryRetainedReads(ctx, account, mutationWatermark, mailconfig.LocalFolders(syncAccount), imapAddress)
	// The cutoff is an initial-import boundary, not a permanent retention
	// boundary. Applying it to every incremental run would silently remove old
	// mail whenever mbsync re-indexed it. Local retention is the only policy
	// that remains active after the first successful import.
	cutoff, err := initialCutoff(account, kind)
	if err != nil {
		return err
	}
	if s.Index != nil {
		pruned, pruneErr := s.Index.PruneLocal(root, cutoff, account.RetentionDays)
		if pruneErr != nil {
			return pruneErr
		}
		changed = changed || pruned
		if err := s.Index.Index(ctx, account.ID, root); err != nil {
			return operation.Wrap("mailindex", "sync", operation.MailIndex, err)
		}
	}
	// mbsync owns ordinary incremental UID/flag propagation. The authenticated
	// remote walk is deliberately reserved for the initial import, scheduled
	// reconciliation, and an explicit manual refresh; doing it every five
	// minutes would turn the incremental worker into a full IMAP scan.
	if shouldReconcileRemote(kind) {
		if err := s.reconcileRemoteIdentity(ctx, account, folderMap, imapAddress, mutationWatermark); err != nil {
			return err
		}
	}
	if err := s.Store.MarkQueuedMutationsSynchronized(ctx, account.ID, mutationWatermark, mailconfig.LocalFolders(syncAccount)); err != nil {
		return fmt.Errorf("record synchronized mail mutations: %w", err)
	}
	if retainedReadErr != nil {
		return fmt.Errorf("synchronize retained read changes: %w", retainedReadErr)
	}
	s.mu.Lock()
	s.last[account.ID] = time.Now()
	s.mu.Unlock()
	if changed && s.Log != nil {
		s.Log.Info("mail sync changed maildir", "account", account.ID, "kind", kind)
	}
	return nil
}

// CheckPublicIMAPEndpoint performs the same globally-routable endpoint check
// used by the web onboarding path. It is intentionally a dial-and-close
// preflight because mbsync owns the actual socket lifecycle.
func CheckPublicIMAPEndpoint(ctx context.Context, host string, port int) (string, error) {
	return netguard.CheckAndPin(ctx, host, port, false)
}

func shouldReconcileRemote(kind string) bool {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "initial", "reconciliation", "manual":
		return true
	default:
		return false
	}
}

func initialCutoff(account store.Account, kind string) (*time.Time, error) {
	if kind != "initial" || account.InitialCutoff == nil || strings.TrimSpace(*account.InitialCutoff) == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(*account.InitialCutoff))
	if err != nil {
		return nil, fmt.Errorf("account %s initial cutoff: %w", account.ID, err)
	}
	return &parsed, nil
}

func (s *Service) discoverSyncFolders(ctx context.Context, account store.Account, imapAddress string) ([]string, error) {
	// Isolated runner tests can omit a secret box. Production supplies one.
	if s.Secrets == nil || strings.TrimSpace(account.IMAPPassword) == "" {
		return nil, nil
	}
	password, err := s.Secrets.Open(account.IMAPPassword)
	if err != nil {
		return nil, fmt.Errorf("open IMAP secret for discovery: %w", err)
	}
	connection, err := remoteimap.Open(ctx, remoteimap.Config{Host: account.IMAPHost, Address: imapAddress, Port: account.IMAPPort, Security: account.IMAPSecurity, Username: account.IMAPUser, Password: password})
	if err != nil {
		return nil, err
	}
	defer connection.Close()
	folders, err := connection.ListFolders()
	if err != nil {
		return nil, fmt.Errorf("discover IMAP folders before sync: %w", err)
	}
	if folders == nil {
		folders = []string{}
	}
	return folders, nil
}

func (s *Service) reconcileRemoteIdentity(ctx context.Context, account store.Account, mapping map[string]string, imapAddress string, mutationWatermark int64) error {
	if s.Secrets == nil || strings.TrimSpace(account.IMAPPassword) == "" {
		return nil
	}
	password, err := s.Secrets.Open(account.IMAPPassword)
	if err != nil {
		return fmt.Errorf("open IMAP secret for identity reconciliation: %w", err)
	}
	port := account.IMAPPort
	if port == 0 {
		port = 993
	}
	connection, err := remoteimap.Open(ctx, remoteimap.Config{Host: account.IMAPHost, Address: imapAddress, Port: port, Security: account.IMAPSecurity, Username: account.IMAPUser, Password: password, OperationTimeout: reconciliationConnectionBudget})
	if err != nil {
		return err
	}
	defer connection.Close()

	// Local read changes that have not reached the provider yet must not be
	// erased when the provider's flags are replayed into the index. A user
	// who listened to a message minutes ago keeps that read state even when
	// the server still reports it unread; mbsync pushes the flag on its next
	// run and a later reconciliation aligns the remainder.
	pendingReads := make(map[string]struct{})
	addPendingRead := func(item store.QueuedReadMutation) {
		if item.UID != 0 && item.UIDValidity != 0 {
			pendingReads[fmt.Sprintf("uid:%d:%d", item.UID, item.UIDValidity)] = struct{}{}
		}
		if strings.TrimSpace(item.MessageID) != "" {
			pendingReads["mid:"+item.MessageID] = struct{}{}
		}
	}
	if mutationWatermark > 0 {
		pending, err := s.Store.QueuedReadMutations(ctx, account.ID, mutationWatermark)
		if err != nil {
			if s.Log != nil {
				s.Log.Warn("mail reconciliation could not load pending read changes", "account_id", account.ID, "error", err)
			}
		} else {
			for _, item := range pending {
				addPendingRead(item)
			}
		}
	}
	// A read change the journal has already handed to mbsync is no longer
	// queued, yet the provider's flag view can still lag that hand-off when
	// reconciliation replays remote flags. Protect the local read state for a
	// grace window after the change was recorded so the replay cannot erase
	// the user's read before the provider catches up.
	cutoff := s.currentTime().Add(-readReconcileGrace).UTC().Truncate(time.Second).Format(time.RFC3339)
	recent, err := s.Store.RecentReadMutations(ctx, account.ID, cutoff)
	if err != nil {
		if s.Log != nil {
			s.Log.Warn("mail reconciliation could not load recent read changes", "account_id", account.ID, "error", err)
		}
	} else {
		for _, item := range recent {
			addPendingRead(item)
		}
	}
	roles, err := s.Store.ListFolderRoles(ctx, account.UserID, account.ID)
	if err != nil {
		return err
	}
	roleByRemote := make(map[string]string, len(roles))
	for _, role := range roles {
		roleByRemote[role.RemotePath] = role.Role
	}
	folders, err := connection.ListFolders()
	if err != nil {
		return fmt.Errorf("discover IMAP folders for reconciliation: %w", err)
	}
	remotePaths := make([]string, 0, len(folders))
	remoteIdentities := make(map[string]struct{})
	for _, remoteFolder := range folders {
		remotePaths = append(remotePaths, remoteFolder)
		localFolder := mapping[remoteFolder]
		if localFolder == "" && strings.EqualFold(remoteFolder, "INBOX") {
			localFolder = "Inbox"
		}
		if localFolder == "" {
			localFolder = remoteFolder
		}
		if err := s.Store.UpsertRemoteFolder(ctx, account.ID, remoteFolder, localFolder, roleByRemote[remoteFolder]); err != nil {
			return err
		}
		identities, _, err := connection.ListMessageIdentities(remoteFolder)
		if err != nil {
			return fmt.Errorf("reconcile IMAP folder %q: %w", remoteFolder, err)
		}
		physicalFolders, err := s.Store.PhysicalFoldersForRemote(ctx, account.UserID, account.ID, remoteFolder)
		if err != nil {
			return err
		}
		for _, identity := range identities {
			// UIDVALIDITY+UID is the authoritative identity when available.
			// Keep a Message-ID alias only as a conservative bridge for index
			// rows created before the next sync recorded their IMAP UID. An alias
			// may preserve a stale duplicate, but it must never cause deletion.
			retainRemoteIdentity(remoteIdentities, physicalFolders, identity)
			keepLocalRead := false
			if _, ok := pendingReads[fmt.Sprintf("uid:%d:%d", identity.UID, identity.UIDValidity)]; ok {
				keepLocalRead = true
			}
			if strings.TrimSpace(identity.MessageID) != "" {
				if _, ok := pendingReads["mid:"+strings.TrimSpace(identity.MessageID)]; ok {
					keepLocalRead = true
				}
			}
			var updateErr error
			if strings.TrimSpace(identity.MessageID) != "" {
				updateErr = s.Store.UpdateMailIdentity(ctx, account.ID, localFolder, identity.MessageID, identity.UID, identity.UIDValidity, identity.Flags, keepLocalRead)
			} else {
				updateErr = s.Store.UpdateMailIdentityByUID(ctx, account.ID, localFolder, identity.UID, identity.UIDValidity, identity.Flags, keepLocalRead)
			}
			if updateErr != nil {
				return updateErr
			}
		}
	}
	if err := s.reconcileIndexedMessages(ctx, account, remoteIdentities); err != nil {
		return fmt.Errorf("reconcile stale indexed messages: %w", err)
	}
	if err := s.Store.PruneRemoteFolders(ctx, account.ID, remotePaths); err != nil {
		return fmt.Errorf("prune stale IMAP folders: %w", err)
	}
	return nil
}

func remoteIdentityKey(folder string, identity remoteimap.MessageIdentity) string {
	folder = strings.TrimSpace(folder)
	if identity.UID != 0 && identity.UIDValidity != 0 {
		return fmt.Sprintf("%s\x00uid\x00%d\x00%d", folder, identity.UIDValidity, identity.UID)
	}
	return remoteMessageIDKey(folder, identity.MessageID)
}

func retainRemoteIdentity(snapshot map[string]struct{}, folders []string, identity remoteimap.MessageIdentity) {
	for _, folder := range folders {
		snapshot[remoteIdentityKey(folder, identity)] = struct{}{}
		if strings.TrimSpace(identity.MessageID) != "" {
			snapshot[remoteMessageIDKey(folder, identity.MessageID)] = struct{}{}
		}
	}
}

func remoteMessageIDKey(folder, messageID string) string {
	return strings.TrimSpace(folder) + "\x00message-id\x00" + strings.TrimSpace(messageID)
}

type mbsyncFolderState struct {
	available      bool
	farUIDValidity uint32
	nearToFar      map[uint32]uint32
}

type indexedMessage struct {
	id                      int64
	path, folder, messageID string
	uid, uidValidity        uint32
	identity                remoteimap.MessageIdentity
}

// readMBSyncFolderState reads the stable mapping written by SyncState *.
// The near-side UID in a Maildir filename is not the remote UID; the state
// file's first mapping column is the far-side UID and its header contains the
// far-side UIDVALIDITY. Malformed state is an error so callers fail closed.
func readMBSyncFolderState(root, folder string) (mbsyncFolderState, error) {
	folderRoot := root
	if strings.TrimSpace(folder) != "" && folder != "." {
		folderRoot = filepath.Join(root, filepath.FromSlash(folder))
	}
	statePath := filepath.Join(folderRoot, ".mbsyncstate")
	if !underSyncRoot(statePath, root) {
		return mbsyncFolderState{}, fmt.Errorf("mbsync state path is outside the account root")
	}
	data, err := os.ReadFile(statePath)
	if os.IsNotExist(err) {
		return mbsyncFolderState{}, nil
	}
	if err != nil {
		return mbsyncFolderState{}, fmt.Errorf("read mbsync state %q: %w", folder, err)
	}
	return parseMBSyncFolderState(data, statePath)
}

func parseMBSyncFolderState(data []byte, statePath string) (mbsyncFolderState, error) {
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	header := make(map[string]uint32)
	blank := -1
	for index, line := range lines {
		if line == "" {
			blank = index
			break
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return mbsyncFolderState{}, fmt.Errorf("malformed mbsync state header in %q", statePath)
		}
		value, err := strconv.ParseUint(fields[1], 10, 32)
		if err != nil {
			return mbsyncFolderState{}, fmt.Errorf("invalid mbsync state header %q in %q", fields[0], statePath)
		}
		if _, exists := header[fields[0]]; exists {
			return mbsyncFolderState{}, fmt.Errorf("duplicate mbsync state header %q in %q", fields[0], statePath)
		}
		header[fields[0]] = uint32(value)
	}
	if blank < 0 {
		return mbsyncFolderState{}, fmt.Errorf("mbsync state %q has no header terminator", statePath)
	}
	farValidity, farOK := header["FarUidValidity"]
	nearValidity, nearOK := header["NearUidValidity"]
	if !farOK || !nearOK {
		return mbsyncFolderState{}, fmt.Errorf("mbsync state %q lacks UIDVALIDITY headers", statePath)
	}
	if farValidity == 0 || nearValidity == 0 {
		return mbsyncFolderState{}, fmt.Errorf("mbsync state %q has invalid UIDVALIDITY headers", statePath)
	}
	_ = nearValidity // The value is validated here; mappings are keyed by near UID.
	nearToFar := make(map[uint32]uint32)
	farSeen := make(map[uint32]struct{})
	for _, line := range lines[blank+1:] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || len(fields) > 3 {
			return mbsyncFolderState{}, fmt.Errorf("malformed mbsync state mapping in %q", statePath)
		}
		far, farErr := strconv.ParseUint(fields[0], 10, 32)
		near, nearErr := strconv.ParseUint(fields[1], 10, 32)
		if farErr != nil || nearErr != nil || far == 0 || near == 0 {
			return mbsyncFolderState{}, fmt.Errorf("invalid mbsync state mapping in %q", statePath)
		}
		farUID, nearUID := uint32(far), uint32(near)
		if _, exists := nearToFar[nearUID]; exists {
			return mbsyncFolderState{}, fmt.Errorf("duplicate near UID %d in %q", nearUID, statePath)
		}
		if _, exists := farSeen[farUID]; exists {
			return mbsyncFolderState{}, fmt.Errorf("duplicate far UID %d in %q", farUID, statePath)
		}
		nearToFar[nearUID] = farUID
		farSeen[farUID] = struct{}{}
	}
	return mbsyncFolderState{available: true, farUIDValidity: farValidity, nearToFar: nearToFar}, nil
}

func maildirNearUID(path string) (uint32, bool) {
	name := filepath.Base(path)
	marker := ",U="
	if strings.Count(name, marker) != 1 {
		return 0, false
	}
	value := name[strings.Index(name, marker)+len(marker):]
	end := strings.Index(value, ":2,")
	if end <= 0 {
		return 0, false
	}
	uid, err := strconv.ParseUint(value[:end], 10, 32)
	return uint32(uid), err == nil && uid != 0
}

func mbsyncIdentityForPath(root, folder, path string, cache map[string]mbsyncFolderState) (remoteimap.MessageIdentity, bool, bool, error) {
	if !underSyncRoot(path, root) {
		return remoteimap.MessageIdentity{}, false, true, nil
	}
	state, loaded := cache[folder]
	if !loaded {
		var err error
		state, err = readMBSyncFolderState(root, folder)
		if err != nil {
			return remoteimap.MessageIdentity{}, false, true, err
		}
		cache[folder] = state
	}
	if !state.available {
		return remoteimap.MessageIdentity{}, false, false, nil
	}
	nearUID, ok := maildirNearUID(path)
	if !ok {
		return remoteimap.MessageIdentity{}, false, true, nil
	}
	farUID, ok := state.nearToFar[nearUID]
	if !ok {
		return remoteimap.MessageIdentity{}, false, true, nil
	}
	return remoteimap.MessageIdentity{Folder: folder, UID: farUID, UIDValidity: state.farUIDValidity}, true, false, nil
}

// reconcileIndexedMessages removes local index/file entries that a complete
// remote identity walk no longer contains. mbsync normally performs this
// cleanup, but Remove None intentionally prevents destructive propagation; a
// scheduled reconciliation must still retire messages deleted remotely while
// never touching a path outside the account's Maildir root.
func (s *Service) reconcileIndexedMessages(ctx context.Context, account store.Account, remote map[string]struct{}) error {
	root := filepath.Join(s.Root, "mail", account.ID)
	rows, err := s.Store.ListIndexedMail(ctx, account.ID)
	if err != nil {
		return err
	}
	var stale []indexedMessage
	var hydrated []indexedMessage
	stateCache := make(map[string]mbsyncFolderState)
	for _, row := range rows {
		item := indexedMessage{id: row.ID, path: row.Path, folder: row.Folder, messageID: row.MessageID, uid: row.UID, uidValidity: row.UIDValidity}
		identity, fromState, unknown, stateErr := mbsyncIdentityForPath(root, item.folder, item.path, stateCache)
		if stateErr != nil {
			// One unreadable or deleted message must not abort the whole
			// reconcile: the remaining messages would stay unsynchronized
			// for as long as that one file keeps failing. Log it and carry
			// on; the next sync retries the same row.
			if s.Log != nil {
				s.Log.Warn("mail reconciliation could not read a message identity", "account_id", account.ID, "path", item.path, "error", stateErr)
			}
			continue
		}
		if unknown {
			continue
		}
		if fromState {
			item.uid, item.uidValidity = identity.UID, identity.UIDValidity
			item.identity = identity
			hydrated = append(hydrated, item)
		} else {
			identity = remoteimap.MessageIdentity{MessageID: item.messageID, UID: item.uid, UIDValidity: item.uidValidity}
			item.identity = identity
		}
		// A row without either a stable provider identity or a Message-ID is
		// unknown, not confirmed absent. Never turn a failed/partial identity
		// import into destructive local deletion.
		if item.uid == 0 && item.uidValidity == 0 && strings.TrimSpace(item.messageID) == "" {
			continue
		}
		if _, ok := remote[remoteIdentityKey(item.folder, item.identity)]; !ok {
			if underSyncRoot(item.path, root) {
				stale = append(stale, item)
			}
		}
	}
	for _, item := range hydrated {
		if err := s.Store.UpdateIndexedMailUID(ctx, account.ID, item.id, item.uid, item.uidValidity); err != nil {
			if s.Log != nil {
				s.Log.Warn("mail reconciliation could not update a message identity", "account_id", account.ID, "path", item.path, "error", err)
			}
			continue
		}
	}
	for _, item := range stale {
		if err := s.cleanupStaleIndexedMessage(ctx, account, root, item); err != nil {
			if s.Log != nil {
				s.Log.Warn("mail reconciliation could not clean up a stale message", "account_id", account.ID, "path", item.path, "error", err)
			}
			continue
		}
	}
	return nil
}

func (s *Service) cleanupStaleIndexedMessage(ctx context.Context, account store.Account, root string, item indexedMessage) error {
	relative, err := filepath.Rel(root, item.path)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("stale mail path is outside account root")
	}
	action := "remove"
	target := ""
	if strings.TrimSpace(s.QuarantineRoot) != "" {
		action = "quarantine"
		quarantineRoot := filepath.Join(s.QuarantineRoot, sanitizeCleanupComponent(account.ID))
		if err := os.MkdirAll(quarantineRoot, 0700); err != nil {
			return err
		}
		batch := fmt.Sprintf("%d-%d", s.currentTime().UnixNano(), item.id)
		target = filepath.Join(quarantineRoot, batch, relative)
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		if !pathWithinExistingRoot(target, quarantineRoot) {
			return fmt.Errorf("quarantine path is outside quarantine root")
		}
	}
	detail, _ := json.Marshal(map[string]any{
		"account_id":   account.ID,
		"message_id":   item.id,
		"path":         filepath.ToSlash(relative),
		"action":       action,
		"target":       filepath.ToSlash(target),
		"uid":          item.uid,
		"uid_validity": item.uidValidity,
	})
	if err := s.Store.Audit(ctx, account.UserID, "mail_reconciliation_cleanup", string(detail)); err != nil {
		return fmt.Errorf("record cleanup decision: %w", err)
	}
	if action == "quarantine" {
		if err := os.Rename(item.path, target); err != nil && !os.IsNotExist(err) {
			return err
		}
	} else if err := os.Remove(item.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := s.Store.DeleteIndexedMail(ctx, account.ID, item.id); err != nil {
		return err
	}
	return nil
}

func sanitizeCleanupComponent(value string) string {
	var b strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "account"
	}
	return b.String()
}

func pathWithinExistingRoot(path, root string) bool {
	pathAbs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return false
	}
	rootAbs, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return false
	}
	pathReal, pathErr := filepath.EvalSymlinks(pathAbs)
	if pathErr != nil && !os.IsNotExist(pathErr) {
		return false
	}
	rootReal, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return false
	}
	if pathErr != nil {
		parent, parentErr := filepath.EvalSymlinks(filepath.Dir(pathAbs))
		if parentErr != nil {
			return false
		}
		pathReal = filepath.Join(parent, filepath.Base(pathAbs))
	}
	return filepath.Clean(pathReal) == filepath.Clean(rootReal) || strings.HasPrefix(filepath.Clean(pathReal), filepath.Clean(rootReal)+string(filepath.Separator))
}

func underSyncRoot(path, root string) bool {
	pathAbs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return false
	}
	rootAbs, err := filepath.Abs(filepath.Clean(root))
	if err != nil || rootAbs == string(filepath.Separator) {
		return false
	}
	rootReal, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return false
	}
	pathReal, err := filepath.EvalSymlinks(pathAbs)
	if err != nil {
		// A missing path can be safely considered in-root only when its
		// existing parent resolves below the real root. This avoids following
		// a symlink that points outside the account while still allowing the
		// database row for an already-deleted file to be retired.
		parentReal, parentErr := filepath.EvalSymlinks(filepath.Dir(pathAbs))
		if parentErr != nil {
			return false
		}
		pathReal = filepath.Join(parentReal, filepath.Base(pathAbs))
	}
	rootReal = filepath.Clean(rootReal)
	pathReal = filepath.Clean(pathReal)
	return pathReal == rootReal || strings.HasPrefix(pathReal, rootReal+string(filepath.Separator))
}
