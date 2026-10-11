package maintenance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/voxmail/voxmail/internal/store"
)

type testClock struct {
	mu      sync.Mutex
	at      time.Time
	ticks   chan time.Time
	ready   chan struct{}
	stopped chan struct{}
}

func newTestClock() *testClock {
	return &testClock{at: time.Now().UTC(), ticks: make(chan time.Time), ready: make(chan struct{}, 16), stopped: make(chan struct{})}
}
func (c *testClock) Now() time.Time            { c.mu.Lock(); defer c.mu.Unlock(); return c.at }
func (c *testClock) Channel() <-chan time.Time { c.ready <- struct{}{}; return c.ticks }
func (c *testClock) Stop()                     { close(c.stopped) }
func (c *testClock) wait(t *testing.T) {
	t.Helper()
	select {
	case <-c.ready:
	case <-time.After(3 * time.Second):
		t.Fatal("scheduled maintenance pass did not complete")
	}
}
func (c *testClock) advance(t *testing.T, by time.Duration) {
	t.Helper()
	c.mu.Lock()
	c.at = c.at.Add(by)
	at := c.at
	c.mu.Unlock()
	select {
	case c.ticks <- at:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not await its next tick")
	}
	c.wait(t)
}

func retentionFixture(t *testing.T, root string) *store.Store {
	t.Helper()
	db, err := store.Open(filepath.Join(root, "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.CreateUser(context.Background(), store.User{ID: "owner", Username: "owner", PasswordHash: "p", PINHash: "p", Role: "admin", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,created_at) VALUES('account','owner','mail','mail@example.com','Owner','imap','u','p','smtp','u','p','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO mail_messages(id,account_id,folder,path,updated_at) VALUES(1,'account','Inbox','/mail/message','now')`); err != nil {
		t.Fatal(err)
	}
	return db
}

func mustExec(t *testing.T, db *store.Store, query string, args ...any) {
	t.Helper()
	if _, err := db.DB.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func TestClockAdvancePrunesTerminalHistoryAndFilesWithoutRestart(t *testing.T) {
	root := t.TempDir()
	db := retentionFixture(t, root)
	clock := newTestClock()
	now := clock.Now()
	recent, ancient := now.Format(time.RFC3339Nano), now.Add(-90*24*time.Hour).Format(time.RFC3339Nano)
	mustExec(t, db, `INSERT INTO audit_log(user_id,action,detail,created_at) VALUES('owner','test','',?)`, recent)
	mustExec(t, db, `INSERT INTO sync_runs(account_id,kind,started_at,finished_at) VALUES('account','initial',?,NULL)`, ancient)
	mustExec(t, db, `INSERT INTO sync_runs(account_id,kind,started_at,finished_at) VALUES('account','manual',?,?)`, ancient, recent)
	for _, status := range []string{"pending", "uncertain", "running", "future-state", "accepted"} {
		stamp := ancient
		if status == "accepted" {
			stamp = recent
		}
		mustExec(t, db, `INSERT INTO outbound_submissions(message_id,user_id,account_id,status,created_at,updated_at) VALUES(?,'owner','account',?,?,?)`, status, status, stamp, stamp)
	}
	for _, status := range []string{"queued", "uncertain", "running", "future-state", "success"} {
		stamp := ancient
		if status == "success" {
			stamp = recent
		}
		mustExec(t, db, `INSERT INTO message_mutations(message_id,user_id,operation,status,created_at) VALUES(1,'owner','test',?,?)`, status, stamp)
	}
	for _, status := range []string{"queued", "running", "complete"} {
		stamp := ancient
		if status == "complete" {
			stamp = recent
		}
		mustExec(t, db, `INSERT INTO voice_install_jobs(id,user_id,voice,stage,status,created_at,updated_at) VALUES(?,'owner','voice',?,?,?,?)`, status, status, status, stamp, stamp)
	}
	for _, status := range []string{"pending", "failed", "complete"} {
		stamp := ancient
		if status == "complete" {
			stamp = recent
		}
		mustExec(t, db, `INSERT INTO cleanup_jobs(id,user_id,account_id,paths,status,created_at,updated_at) VALUES(?,'owner','account','[]',?,?,?)`, status, status, stamp, stamp)
	}
	recordings := filepath.Join(root, "recordings")
	if err := os.MkdirAll(recordings, 0700); err != nil {
		t.Fatal(err)
	}
	active, release, err := CreateTemp(recordings, ".recording-active-")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	_ = active.Close()
	abandoned := filepath.Join(recordings, ".recording-abandoned")
	if err := os.WriteFile(abandoned, []byte("abandoned"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{active.Name(), abandoned} {
		if err := os.Chtimes(path, now, now); err != nil {
			t.Fatal(err)
		}
	}
	cleaner, err := NewCleaner(root, DefaultEphemeralMaxAge)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	worker := &Worker{Store: db, Files: cleaner, Log: slog.New(slog.NewTextHandler(&logs, nil)), Policy: store.HistoryRetentionPolicy{MaxAge: 7 * 24 * time.Hour, MaxRows: 100}, now: clock.Now, newTicker: func(time.Duration) maintenanceTicker { return clock }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	clock.wait(t)
	var count int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM sync_runs`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("recently completed and active syncs must survive: count=%d err=%v", count, err)
	}
	mustExec(t, db, `CREATE TRIGGER fail_audit_prune BEFORE DELETE ON audit_log BEGIN SELECT RAISE(ABORT,'injected retention failure'); END`)
	clock.advance(t, 8*24*time.Hour)
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("injected failure was not observed: count=%d err=%v", count, err)
	}
	if !bytes.Contains(logs.Bytes(), []byte("injected retention failure")) {
		t.Fatal("maintenance failure was hidden from operators")
	}
	mustExec(t, db, `DROP TRIGGER fail_audit_prune`)
	clock.advance(t, DefaultInterval)
	for table, want := range map[string]int{"audit_log": 0, "sync_runs": 1, "outbound_submissions": 4, "message_mutations": 4, "voice_install_jobs": 2, "cleanup_jobs": 2} {
		if err := db.DB.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != want {
			t.Errorf("%s count=%d want=%d err=%v", table, count, want, err)
		}
	}
	if _, err := os.Stat(abandoned); !os.IsNotExist(err) {
		t.Errorf("expired abandoned recording remains: %v", err)
	}
	if _, err := os.Stat(active.Name()); err != nil {
		t.Fatalf("active recording was removed after clock advance: %v", err)
	}
	release()
	release() // Idempotent, including the deferred release.
	clock.advance(t, DefaultInterval)
	if _, err := os.Stat(active.Name()); !os.IsNotExist(err) {
		t.Errorf("released recording was not cleaned: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("maintenance did not stop before database close")
	}
	select {
	case <-clock.stopped:
	default:
		t.Error("ticker not stopped")
	}
	for _, rule := range cleaner.rules {
		if rule.reader != nil {
			t.Error("directory handle remains after shutdown")
		}
	}
}

func TestCleanupBudgetsResumePastUnrelatedEntriesAndLargeStagingTree(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	recordings := filepath.Join(root, "recordings")
	drafts := filepath.Join(root, "drafts")
	for _, dir := range []string{recordings, drafts} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 4*CleanupEntriesPerDirectory; i++ {
		if err := os.WriteFile(filepath.Join(recordings, fmt.Sprintf("user-audio-%04d", i)), []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	old := filepath.Join(recordings, ".recording-late")
	if err := os.WriteFile(old, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(drafts, ".draft-stage-large")
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4*CleanupTreeNodes; i++ {
		if err := os.WriteFile(filepath.Join(stage, fmt.Sprintf("%04d.bin", i)), []byte("stage"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{old, stage} {
		if err := os.Chtimes(path, now.Add(-48*time.Hour), now.Add(-48*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	c, err := NewCleaner(root, DefaultEphemeralMaxAge)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i := 0; i < 12; i++ {
		report, err := c.Clean(context.Background(), now)
		if err != nil {
			t.Fatal(err)
		}
		if report.Visited > 2*CleanupEntriesPerDirectory+CleanupTreeNodes+10 || report.RemovedFiles > CleanupEntriesPerDirectory+CleanupTreeNodes {
			t.Fatalf("unbounded pass: %+v", report)
		}
	}
	for _, path := range []string{old, stage} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("cursor/partial-tree progress failed for %s: %v", path, err)
		}
	}
	entries, err := os.ReadDir(recordings)
	if err != nil || len(entries) != 4*CleanupEntriesPerDirectory {
		t.Errorf("user data affected: entries=%d err=%v", len(entries), err)
	}
}

func TestCleanupProtectsStagedTreeAndNativeAudioDirectory(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC().Add(48 * time.Hour)
	drafts := filepath.Join(root, "drafts")
	audio := filepath.Join(root, "run", "voxmail")
	for _, dir := range []string{drafts, audio} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	stage, releaseStage, err := MkdirTemp(drafts, ".draft-stage-")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseStage()
	child := filepath.Join(stage, "attachment.bin")
	if err := os.WriteFile(child, []byte("active draft"), 0600); err != nil {
		t.Fatal(err)
	}
	releaseAudio := Protect(audio)
	defer releaseAudio()
	path := filepath.Join(audio, "active.rx.pcm")
	if err := os.WriteFile(path, []byte("capture"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := CleanupEphemeral(root, now, time.Hour); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{child, path} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("active owner lost %s: %v", path, err)
		}
	}
	releaseStage()
	releaseAudio()
	if _, err := CleanupEphemeral(root, now, time.Hour); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{stage, path} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("released owner was not cleaned %s: %v", path, err)
		}
	}
}

func TestCleanupCancellationAndSymlinkedParent(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "mbsync"), 0700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(outside, "mbsync", ".conf-private")
	if err := os.WriteFile(keep, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "config")); err != nil {
		t.Fatal(err)
	}
	c, err := NewCleaner(root, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if report, err := c.Clean(ctx, time.Now().Add(48*time.Hour)); !errors.Is(err, context.Canceled) || report.Visited != 0 {
		t.Fatalf("canceled pass did work: %+v %v", report, err)
	}
	if _, err := c.Clean(context.Background(), time.Now().Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal("cleanup followed parent symlink", err)
	}
}

func TestPeriodicCleanupExpiresCacheButPreservesPlaybackAndStaticPrompts(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "prompts", "dynamic")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	active := filepath.Join(dir, "active.wav")
	stale := filepath.Join(dir, "stale.wav")
	recent := filepath.Join(dir, "recent.wav")
	static := filepath.Join(root, "prompts", "welcome.wav")
	for _, path := range []string{active, stale, recent, static} {
		if err := os.WriteFile(path, []byte("audio"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{active, stale, static} {
		if err := os.Chtimes(path, now.Add(-8*24*time.Hour), now.Add(-8*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	release := Protect(active)
	defer release()
	c, err := NewCleaner(root, DefaultEphemeralMaxAge)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Clean(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("expired cached speech remains", err)
	}
	for _, path := range []string{active, recent, static} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal("cleanup removed active/recent/static speech", err)
		}
	}
	release()
	if _, err := c.Clean(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(active); !os.IsNotExist(err) {
		t.Fatal("expired cache lease was not released", err)
	}
}

func TestConfiguredRootAliasSharesOwnershipBeforePublication(t *testing.T) {
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "data")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(alias, "prompts", "dynamic", "new.wav")
	release := Protect(path)
	defer release()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("published"), 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(8 * 24 * time.Hour)
	if report, err := CleanupEphemeral(alias, now, DefaultEphemeralMaxAge); err != nil || report.RemovedFiles != 0 {
		t.Fatalf("aliased publication lost ownership: %+v %v", report, err)
	}
	release()
	if _, err := CleanupEphemeral(alias, now, DefaultEphemeralMaxAge); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("aliased abandoned publication not cleaned", err)
	}
}

type blockedStore struct {
	entered  chan struct{}
	canceled chan struct{}
	release  chan struct{}
	history  chan struct{}
}

func (s *blockedStore) PruneEphemeral(ctx context.Context, _ time.Time) error {
	close(s.entered)
	<-ctx.Done()
	close(s.canceled)
	<-s.release
	return ctx.Err()
}
func (s *blockedStore) PruneHistory(context.Context, time.Time, store.HistoryRetentionPolicy) error {
	s.history <- struct{}{}
	return nil
}

func TestWorkerCancelsAndAwaitsItsInFlightPass(t *testing.T) {
	s := &blockedStore{entered: make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{}), history: make(chan struct{}, 1)}
	clock := newTestClock()
	w := &Worker{Store: s, newTicker: func(time.Duration) maintenanceTicker { return clock }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	select {
	case <-s.entered:
	case <-time.After(time.Second):
		t.Fatal("maintenance did not begin")
	}
	cancel()
	select {
	case <-s.canceled:
	case <-time.After(time.Second):
		t.Fatal("maintenance operation did not receive lifecycle cancellation")
	}
	select {
	case err := <-done:
		t.Fatalf("worker returned before its operation finished: %v", err)
	default:
	}
	close(s.release)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not await shutdown")
	}
	select {
	case <-s.history:
		t.Fatal("worker started another operation after cancellation")
	default:
	}
	select {
	case <-clock.stopped:
	default:
		t.Fatal("worker did not stop its timer")
	}
}

func TestWorkerPassDeadlineCancelsBlockedOperation(t *testing.T) {
	s := &blockedStore{entered: make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{}), history: make(chan struct{}, 1)}
	clock := newTestClock()
	w := &Worker{Store: s, PassTimeout: 20 * time.Millisecond, newTicker: func(time.Duration) maintenanceTicker { return clock }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	select {
	case <-s.canceled:
	case <-time.After(time.Second):
		t.Fatal("maintenance pass had no deadline")
	}
	close(s.release)
	clock.wait(t)
	select {
	case <-s.history:
		t.Fatal("expired pass started history work")
	default:
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}
}
