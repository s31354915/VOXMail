package mailsync

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	remoteimap "github.com/voxmail/voxmail/internal/imap"
	"github.com/voxmail/voxmail/internal/mailconfig"
	"github.com/voxmail/voxmail/internal/mailindex"
	"github.com/voxmail/voxmail/internal/netguard"
	"github.com/voxmail/voxmail/internal/secret"
	"github.com/voxmail/voxmail/internal/store"
)

type Service struct {
	Store   *store.Store
	Root    string
	Runner  Runner
	Log     *slog.Logger
	Index   *mailindex.Indexer
	Secrets *secret.Box
	// EndpointChecker is required in production before mbsync is launched.
	// Tests may leave it nil when the Runner is a local fake.
	EndpointChecker func(context.Context, string, int) error
	mu              sync.Mutex
	busy            map[string]bool
	last            map[string]time.Time
	running         atomic.Bool
}

func (s *Service) Run(ctx context.Context) error {
	if s.Store == nil || s.Root == "" {
		return fmt.Errorf("sync store and root are required")
	}
	s.busy = make(map[string]bool)
	s.last = make(map[string]time.Time)
	s.running.Store(true)
	defer s.running.Store(false)
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

func (s *Service) SyncAll(ctx context.Context) error {
	s.ensureState()
	accounts, err := s.Store.ListAllAccounts(ctx)
	if err != nil {
		return err
	}
	for _, account := range accounts {
		if err := s.syncAccount(ctx, account, s.nextKind(ctx, account)); err != nil && s.Log != nil {
			s.Log.Error("mail sync failed", "account", account.ID, "error", err)
		}
	}
	return nil
}

func (s *Service) SyncDue(ctx context.Context) error {
	s.ensureState()
	accounts, err := s.Store.ListAllAccounts(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
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
		due := last.IsZero() || now.Sub(last) >= interval
		if due {
			if err := s.syncAccount(ctx, account, s.nextKind(ctx, account)); err != nil && s.Log != nil {
				s.Log.Error("mail sync failed", "account", account.ID, "error", err)
			}
		}
	}
	return nil
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

func (s *Service) ensureState() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy == nil {
		s.busy = make(map[string]bool)
	}
	if s.last == nil {
		s.last = make(map[string]time.Time)
	}
}

func (s *Service) nextKind(ctx context.Context, account store.Account) string {
	run, err := s.Store.LatestSyncRun(ctx, account.ID, true)
	if err != nil {
		return "initial"
	}
	if run.Kind == "initial" {
		return "incremental"
	}
	interval := time.Duration(account.ReconciliationIntervalMinutes) * time.Minute
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	if parsed, err := time.Parse(time.RFC3339Nano, run.FinishedAt); err == nil && time.Since(parsed) >= interval {
		return "reconciliation"
	}
	return "incremental"
}

func (s *Service) syncAccount(ctx context.Context, account store.Account, kind string) (retErr error) {
	s.mu.Lock()
	if s.busy[account.ID] {
		s.mu.Unlock()
		return nil
	}
	s.busy[account.ID] = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.busy, account.ID); s.mu.Unlock() }()
	runID, err := s.Store.BeginSyncRun(ctx, account.ID, kind)
	if err != nil {
		return err
	}
	changed := false
	defer func() {
		if finishErr := s.Store.FinishSyncRun(context.Background(), runID, retErr == nil, changed, retErr); finishErr != nil && retErr == nil {
			retErr = finishErr
		}
	}()
	root := filepath.Join(s.Root, "mail", account.ID)
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	if s.EndpointChecker != nil {
		port := account.IMAPPort
		if port == 0 {
			port = 993
		}
		if err := s.EndpointChecker(ctx, account.IMAPHost, port); err != nil {
			return fmt.Errorf("IMAP endpoint rejected by outbound policy: %w", err)
		}
	}
	var folderMap map[string]string
	if account.FolderMap != "" {
		_ = json.Unmarshal([]byte(account.FolderMap), &folderMap)
	}
	configText, err := mailconfig.Generate(mailconfig.Account{ID: account.ID, IMAPHost: account.IMAPHost, IMAPPort: account.IMAPPort, IMAPSecurity: account.IMAPSecurity, IMAPUser: account.IMAPUser, MaildirRoot: root, FolderMap: folderMap})
	if err != nil {
		return err
	}
	configDir := filepath.Join(s.Root, "config", "mbsync")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return err
	}
	configPath := filepath.Join(configDir, account.ID+".conf")
	tmp, err := os.CreateTemp(configDir, ".conf-")
	if err != nil {
		return err
	}
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
	channels := mailconfig.ChannelNames(mailconfig.Account{ID: account.ID, IMAPHost: account.IMAPHost, IMAPPort: account.IMAPPort, IMAPSecurity: account.IMAPSecurity, IMAPUser: account.IMAPUser, MaildirRoot: root, FolderMap: folderMap})
	if err := s.Runner.Validate(ctx, configPath, channels...); err != nil {
		return fmt.Errorf("validate mbsync configuration: %w", err)
	}
	result, err := s.Runner.SyncChannels(ctx, configPath, channels...)
	if err != nil {
		return err
	}
	changed = result.Changed
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
			return err
		}
	}
	// mbsync owns ordinary incremental UID/flag propagation. The authenticated
	// remote walk is deliberately reserved for the initial import, scheduled
	// reconciliation, and an explicit manual refresh; doing it every five
	// minutes would turn the incremental worker into a full IMAP scan.
	if shouldReconcileRemote(kind) {
		if err := s.reconcileRemoteIdentity(ctx, account, folderMap); err != nil {
			return err
		}
	}
	if err := s.Store.MarkQueuedMutationsSynchronized(ctx, account.ID); err != nil {
		return fmt.Errorf("record synchronized mail mutations: %w", err)
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
func CheckPublicIMAPEndpoint(ctx context.Context, host string, port int) error {
	conn, err := netguard.DialContext(ctx, host, port, "", false)
	if err != nil {
		return err
	}
	return conn.Close()
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

func (s *Service) reconcileRemoteIdentity(ctx context.Context, account store.Account, mapping map[string]string) error {
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
	connection, err := remoteimap.Open(ctx, remoteimap.Config{Host: account.IMAPHost, Port: port, Security: account.IMAPSecurity, Username: account.IMAPUser, Password: password})
	if err != nil {
		return err
	}
	defer connection.Close()
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
		for _, identity := range identities {
			remoteIdentities[remoteIdentityKey(localFolder, identity)] = struct{}{}
			var updateErr error
			if strings.TrimSpace(identity.MessageID) != "" {
				updateErr = s.Store.UpdateMailIdentity(ctx, account.ID, localFolder, identity.MessageID, identity.UID, identity.UIDValidity, identity.Flags)
			} else {
				updateErr = s.Store.UpdateMailIdentityByUID(ctx, account.ID, localFolder, identity.UID, identity.UIDValidity, identity.Flags)
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
	if messageID := strings.TrimSpace(identity.MessageID); messageID != "" {
		return folder + "\x00message-id\x00" + messageID
	}
	return fmt.Sprintf("%s\x00uid\x00%d\x00%d", folder, identity.UIDValidity, identity.UID)
}

// reconcileIndexedMessages removes local index/file entries that a complete
// remote identity walk no longer contains. mbsync normally performs this
// cleanup, but Remove None intentionally prevents destructive propagation; a
// scheduled reconciliation must still retire messages deleted remotely while
// never touching a path outside the account's Maildir root.
func (s *Service) reconcileIndexedMessages(ctx context.Context, account store.Account, remote map[string]struct{}) error {
	rows, err := s.Store.DB.QueryContext(ctx, `SELECT id,path,folder,COALESCE(message_id,''),COALESCE(imap_uid,0),COALESCE(uid_validity,0) FROM mail_messages WHERE account_id=?`, account.ID)
	if err != nil {
		return err
	}
	type indexed struct {
		id                      int64
		path, folder, messageID string
		uid, uidValidity        uint32
	}
	var stale []indexed
	for rows.Next() {
		var item indexed
		if err := rows.Scan(&item.id, &item.path, &item.folder, &item.messageID, &item.uid, &item.uidValidity); err != nil {
			rows.Close()
			return err
		}
		identity := remoteimap.MessageIdentity{MessageID: item.messageID, UID: item.uid, UIDValidity: item.uidValidity}
		if _, ok := remote[remoteIdentityKey(item.folder, identity)]; !ok {
			stale = append(stale, item)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	root := filepath.Join(s.Root, "mail", account.ID)
	for _, item := range stale {
		if underSyncRoot(item.path, root) {
			if err := os.Remove(item.path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		if _, err := s.Store.DB.ExecContext(ctx, `DELETE FROM mail_messages WHERE id=? AND account_id=?`, item.id, account.ID); err != nil {
			return err
		}
	}
	return nil
}

func underSyncRoot(path, root string) bool {
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	return root != "." && (path == root || strings.HasPrefix(path, root+string(filepath.Separator)))
}
