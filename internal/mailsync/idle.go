package mailsync

import (
	"context"
	"crypto/tls"
	"fmt"
	"strings"
	"sync"
	"time"

	remoteimap "github.com/voxmail/voxmail/internal/imap"
	"github.com/voxmail/voxmail/internal/store"
)

// IDLE watching bounds and cadence. The IMAP IDLE session itself is bounded
// inside internal/imap; the connection budget below leaves room for login,
// folder selection, and logout around a full session.
const (
	idleSessionBudget    = 24 * time.Minute
	idleConnectionBudget = idleSessionBudget + 5*time.Minute
	// idleReconcileEvery decides how often the watcher set is reconciled
	// against the account table: new accounts start watching, removed or
	// reconfigured accounts are restarted.
	idleReconcileEvery = time.Minute
	idleMinBackoff     = time.Minute
	idleMaxBackoff     = 10 * time.Minute
)

// idleWatchState tracks one running per-account watcher so account changes
// can stop exactly the affected goroutine.
type idleWatchState struct {
	cancel    context.CancelFunc
	signature string
}

// idleWatcher keeps one IMAP IDLE connection per account so new INBOX mail
// triggers a targeted sync without waiting for the next scheduled sweep. It
// never replaces polling: SyncDue keeps its schedule, and every watcher
// failure falls back to it, so a provider without IDLE support or a flaky
// connection only costs a quiet reconnect loop, never missed mail.
type idleWatcher struct {
	service *Service
	// onIdle is the production trigger when nil; tests inject their own to
	// observe notifications without running a real sync.
	onIdle func(context.Context, store.Account)
	// reconcileEvery overrides idleReconcileEvery in tests.
	reconcileEvery time.Duration
	// testTLS pins the fake provider's certificate in tests; production
	// leaves it nil so the connection derives its TLS settings from the
	// account's host.
	testTLS *tls.Config
	// testAllowPrivate lets tests point the watcher at a loopback fake
	// provider; production leaves it false so the endpoint policy applies.
	testAllowPrivate bool

	mu     sync.Mutex
	states map[string]idleWatchState
}

// idleSignature identifies the account configuration a watcher is bound to.
// Any change restarts the watcher so new credentials or endpoints take over
// the connection.
func idleSignature(account store.Account) string {
	return strings.Join([]string{
		account.IMAPHost,
		fmt.Sprint(account.IMAPPort),
		account.IMAPUser,
		account.IMAPSecurity,
		fmt.Sprint(account.ConfigVersion),
	}, "\x00")
}

func newIdleWatcher(s *Service) *idleWatcher {
	return &idleWatcher{service: s, states: make(map[string]idleWatchState), reconcileEvery: idleReconcileEvery}
}

// startIdleWatch launches the account reconciler for the watcher set. It
// returns immediately; the watchers own their lifetimes and stop when ctx
// does.
func (s *Service) startIdleWatch(ctx context.Context) *idleWatcher {
	watcher := newIdleWatcher(s)
	watcher.onIdle = s.idleTrigger
	go watcher.reconcileLoop(ctx)
	return watcher
}

func (w *idleWatcher) reconcileLoop(ctx context.Context) {
	every := w.reconcileEvery
	if every <= 0 {
		every = idleReconcileEvery
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	w.reconcile(ctx)
	for {
		select {
		case <-ctx.Done():
			w.stopAll()
			return
		case <-ticker.C:
			w.reconcile(ctx)
		}
	}
}

func (w *idleWatcher) stopAll() {
	w.mu.Lock()
	states := w.states
	w.states = make(map[string]idleWatchState)
	w.mu.Unlock()
	for _, state := range states {
		state.cancel()
	}
}

func (w *idleWatcher) reconcile(ctx context.Context) {
	s := w.service
	if s.Store == nil {
		return
	}
	accounts, err := s.Store.ListAllAccounts(ctx)
	if err != nil {
		if s.Log != nil {
			s.Log.Warn("idle watcher could not list accounts", "error", err)
		}
		return
	}
	want := make(map[string]store.Account, len(accounts))
	for _, account := range accounts {
		if account.IMAPHost == "" || account.IMAPUser == "" || account.IMAPPassword == "" {
			continue
		}
		want[account.ID] = account
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for id, state := range w.states {
		account, ok := want[id]
		if !ok || state.signature != idleSignature(account) {
			state.cancel()
			delete(w.states, id)
		}
	}
	for id, account := range want {
		if _, ok := w.states[id]; ok {
			continue
		}
		watchCtx, cancel := context.WithCancel(ctx)
		w.states[id] = idleWatchState{cancel: cancel, signature: idleSignature(account)}
		go w.watchAccount(watchCtx, account)
	}
}

func (w *idleWatcher) watchAccount(ctx context.Context, account store.Account) {
	s := w.service
	backoff := idleMinBackoff
	for {
		if ctx.Err() != nil {
			break
		}
		triggered, err := w.idleOnce(ctx, account)
		if ctx.Err() != nil {
			break
		}
		if err != nil {
			if s.Log != nil {
				s.Log.Warn("IMAP IDLE watch failed for account; polling continues",
					"account", account.ID, "retry_in", backoff.String(), "error", err)
			}
			select {
			case <-ctx.Done():
			case <-time.After(backoff):
			}
			if backoff < idleMaxBackoff {
				backoff *= 2
			}
			continue
		}
		backoff = idleMinBackoff
		if triggered && w.onIdle != nil {
			w.onIdle(ctx, account)
		}
	}
	w.mu.Lock()
	if state, ok := w.states[account.ID]; ok {
		state.cancel()
		delete(w.states, account.ID)
	}
	w.mu.Unlock()
}

// idleOnce runs a single bounded IDLE session for one account. triggered is
// true only when the server reported a mailbox update on the watched folder.
func (w *idleWatcher) idleOnce(ctx context.Context, account store.Account) (bool, error) {
	s := w.service
	if s.Secrets == nil {
		return false, fmt.Errorf("IMAP credentials unavailable")
	}
	password, err := s.Secrets.Open(account.IMAPPassword)
	if err != nil {
		return false, fmt.Errorf("open IMAP secret for idle watch: %w", err)
	}
	// The endpoint resolver applies the same policy check and numeric
	// pinning as every other IMAP connection in the service. Without one,
	// leave the address empty so the connection resolves the account host
	// under the same outbound policy.
	address := ""
	if s.EndpointResolver != nil {
		resolved, err := s.EndpointResolver(ctx, account.IMAPHost, account.IMAPPort)
		if err != nil {
			return false, fmt.Errorf("resolve IMAP endpoint for idle watch: %w", err)
		}
		address = resolved
	}
	connection, err := remoteimap.Open(ctx, remoteimap.Config{
		Host:             account.IMAPHost,
		Address:          address,
		Port:             account.IMAPPort,
		Security:         account.IMAPSecurity,
		Username:         account.IMAPUser,
		Password:         password,
		OperationTimeout: idleConnectionBudget,
		TLSConfig:        w.testTLS,
		AllowPrivate:     w.testAllowPrivate,
	})
	if err != nil {
		return false, err
	}
	defer connection.Close()
	return connection.Idle(ctx, "INBOX")
}

// idleTrigger is the production hook behind an IDLE notification: a guarded
// targeted sync of the account whose INBOX reported new mail.
func (s *Service) idleTrigger(ctx context.Context, account store.Account) {
	// A fresh account has not had its initial pass yet; the scheduled sweep
	// owns that first run because the initial cutoff prune must not be
	// skipped.
	run, err := s.Store.LatestSyncRun(ctx, account.ID, true)
	if err != nil || run.Kind == "initial" {
		return
	}
	if s.SyncRunning(account.ID) {
		// A sync is already in flight; it will pick the new mail up.
		return
	}
	if err := s.syncAccount(ctx, account, "incremental"); err != nil {
		if s.Log != nil {
			s.Log.Warn("IMAP IDLE triggered sync failed", "account", account.ID, "error", err)
		}
	}
}
