package mailsync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	remoteimap "github.com/voxmail/voxmail/internal/imap"
	"github.com/voxmail/voxmail/internal/mailindex"
	"github.com/voxmail/voxmail/internal/secret"
	"github.com/voxmail/voxmail/internal/store"
)

func TestDefaultAccountToSyncIntegrationPath(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "voxmail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.CreateUser(ctx, store.User{ID: "sync-user", Username: "sync-user", PasswordHash: "p", PINHash: "p", Role: "user", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	box, err := secret.New("sync-test-key-with-more-than-32-characters-123456")
	if err != nil {
		t.Fatal(err)
	}
	account := store.Account{
		ID: "default-account", UserID: "sync-user", CanonicalName: "Default", Email: "user@example.com", SenderName: "User",
		IMAPHost: "imap.example", IMAPUser: "user", IMAPPassword: "imap-password",
		SMTPHost: "smtp.example", SMTPUser: "user", SMTPPassword: "smtp-password", FolderMap: `{}`,
	}
	if err := db.SaveAccount(ctx, box, account); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	runnerPath := filepath.Join(t.TempDir(), "fake-mbsync")
	runner := `#!/bin/sh
set -eu
if [ "$1" = "--list" ]; then
  exit 0
fi
config="$2"
root=$(awk '$1 == "Path" {gsub(/"/, "", $2); print $2; exit}' "$config")
mkdir -p "$root/Inbox/new"
printf '%s\n' 'From: sender@example.com' 'To: user@example.com' 'Subject: synchronized' 'Message-ID: <sync@example.com>' 'Date: Thu, 01 Jan 2026 00:00:00 +0000' '' 'Hello from the sync fixture.' > "$root/Inbox/new/1:2,"
`
	if err := os.WriteFile(runnerPath, []byte(runner), 0700); err != nil {
		t.Fatal(err)
	}

	service := &Service{
		Store: db, Root: root,
		Runner: Runner{Binary: runnerPath},
		Index:  &mailindex.Indexer{Store: db},
		EndpointResolver: func(_ context.Context, host string, port int) (string, error) {
			if host != "imap.example" || port != 993 {
				t.Fatalf("resolver received %q:%d, want imap.example:993", host, port)
			}
			return "8.8.8.8:993", nil
		},
	}
	if err := service.SyncAll(ctx); err != nil {
		t.Fatal(err)
	}
	run, err := db.LatestSyncRun(ctx, account.ID, true)
	if err != nil || !run.Success || run.Kind != "initial" {
		failed, failedErr := db.LatestSyncRun(ctx, account.ID, false)
		t.Fatalf("sync run=%+v err=%v, latest run=%+v failed lookup=%v, want successful initial run", run, err, failed, failedErr)
	}
	messages, err := db.ListMail(ctx, account.UserID, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Subject != "synchronized" {
		t.Fatalf("indexed default-account messages=%+v, want one synchronized message", messages)
	}
}

func TestSyncAllReturnsAccountFailure(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "voxmail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.CreateUser(ctx, store.User{ID: "sync-failure-user", Username: "sync-failure-user", PasswordHash: "p", PINHash: "p", Role: "user", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	box, err := secret.New("sync-failure-test-key-with-more-than-32-characters-123456")
	if err != nil {
		t.Fatal(err)
	}
	account := store.Account{
		ID: "failed-account", UserID: "sync-failure-user", CanonicalName: "Failed", Email: "failed@example.com", SenderName: "Failed",
		IMAPHost: "imap.example", IMAPUser: "user", IMAPPassword: "imap-password",
		SMTPHost: "smtp.example", SMTPUser: "user", SMTPPassword: "smtp-password", FolderMap: `{}`,
	}
	if err := db.SaveAccount(ctx, box, account); err != nil {
		t.Fatal(err)
	}

	service := &Service{Store: db, Root: t.TempDir()}
	if err := service.SyncAll(ctx); err == nil {
		t.Fatal("SyncAll returned nil for failed account")
	} else if !strings.Contains(err.Error(), account.ID) {
		t.Fatalf("SyncAll error = %v, want account ID", err)
	}
	run, err := db.LatestSyncRun(ctx, account.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if run.Success {
		t.Fatalf("failed sync run was marked successful: %+v", run)
	}
	if !strings.Contains(run.Error, "IMAP endpoint resolver is required") {
		t.Fatalf("unexpected durable sync error: %q", run.Error)
	}
}

func TestInitialCutoffOnlyAppliesToInitialRun(t *testing.T) {
	value := "2025-01-02T03:04:05Z"
	account := store.Account{ID: "account-1", InitialCutoff: &value}
	cutoff, err := initialCutoff(account, "initial")
	if err != nil {
		t.Fatal(err)
	}
	if cutoff == nil || cutoff.UTC().Format("2006-01-02T15:04:05Z07:00") != value {
		t.Fatalf("cutoff = %v, want %s", cutoff, value)
	}
	for _, kind := range []string{"incremental", "reconciliation", "manual"} {
		cutoff, err := initialCutoff(account, kind)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if cutoff != nil {
			t.Fatalf("%s unexpectedly applied initial cutoff %v", kind, cutoff)
		}
	}
}

func TestStopAccountRejectsLateSynchronization(t *testing.T) {
	service := &Service{}
	if err := service.StopAccount(context.Background(), "account-being-deleted"); err != nil {
		t.Fatal(err)
	}
	if err := service.syncAccount(context.Background(), store.Account{ID: "account-being-deleted"}, "manual"); err != store.ErrAccountDeleting {
		t.Fatalf("late sync error = %v, want store.ErrAccountDeleting", err)
	}
}

func TestInitialCutoffRejectsMalformedValueOnlyWhenUsed(t *testing.T) {
	value := "not-a-date"
	account := store.Account{ID: "account-1", InitialCutoff: &value}
	if _, err := initialCutoff(account, "incremental"); err != nil {
		t.Fatalf("incremental run should not parse initial cutoff: %v", err)
	}
	if _, err := initialCutoff(account, "initial"); err == nil {
		t.Fatal("malformed initial cutoff was accepted")
	}
}

func TestRemoteReconciliationIsNotRunOnIncrementalSync(t *testing.T) {
	for _, kind := range []string{"initial", "reconciliation", "manual"} {
		if !shouldReconcileRemote(kind) {
			t.Fatalf("%s should run remote reconciliation", kind)
		}
	}
	for _, kind := range []string{"incremental", "", "unknown"} {
		if shouldReconcileRemote(kind) {
			t.Fatalf("%s should not run a full remote walk", kind)
		}
	}
}

func TestNextKindUsesLastFullSyncInsteadOfLastIncremental(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('u','u','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at,reconciliation_interval_minutes) VALUES('a','u','Work','work@example.com','Work','imap.example','user','sealed','smtp.example','user','sealed','{}','now',60)`); err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339Nano)
	recent := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339Nano)
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO sync_runs(account_id,kind,started_at,finished_at,success,changed,error) VALUES('a','reconciliation',?,?,1,0,'')`, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO sync_runs(account_id,kind,started_at,finished_at,success,changed,error) VALUES('a','incremental',?,?,1,0,'')`, recent, recent); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	service := &Service{Store: db, now: func() time.Time { return now }}
	kind := service.nextKind(ctx, store.Account{ID: "a", ReconciliationIntervalMinutes: 60})
	if kind != "reconciliation" {
		t.Fatalf("next kind=%q, want reconciliation", kind)
	}
}

func TestFailedRunUsesBoundedRetryBackoff(t *testing.T) {
	started := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	run := store.SyncRun{StartedAt: started.Format(time.RFC3339Nano), FinishedAt: started.Add(time.Minute).Format(time.RFC3339Nano)}
	if !failedRunInBackoff(run, started.Add(2*time.Minute)) {
		t.Fatal("recent failed run was not held in retry backoff")
	}
	if failedRunInBackoff(run, started.Add(6*time.Minute)) {
		t.Fatal("failed run remained in backoff past the five-minute bound")
	}
	if failedRunInBackoff(store.SyncRun{Success: true, FinishedAt: run.FinishedAt}, started.Add(2*time.Minute)) {
		t.Fatal("successful run entered failed-run backoff")
	}
	if failedRunInBackoff(store.SyncRun{FinishedAt: "not-a-time"}, started.Add(2*time.Minute)) {
		t.Fatal("malformed run timestamp incorrectly suppressed retry")
	}
}

func TestNextKindSurvivesIncrementalHistoryAndServiceRestart(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('u-restart','restart','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at,reconciliation_interval_minutes) VALUES('a-restart','u-restart','Work','work@example.com','Work','imap.example','user','sealed','smtp.example','user','sealed','{}','now',1440)`); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	full := now.Add(-72 * time.Hour).Format(time.RFC3339Nano)
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO sync_runs(account_id,kind,started_at,finished_at,success,changed,error) VALUES('a-restart','reconciliation',?,?,1,0,'')`, full, full); err != nil {
		t.Fatal(err)
	}
	for hour := 1; hour <= 72; hour++ {
		stamp := now.Add(-72*time.Hour + time.Duration(hour)*time.Hour)
		if hour == 72 {
			stamp = now.Add(-5 * time.Minute)
		}
		value := stamp.Format(time.RFC3339Nano)
		if _, err := db.DB.ExecContext(ctx, `INSERT INTO sync_runs(account_id,kind,started_at,finished_at,success,changed,error) VALUES('a-restart','incremental',?,?,1,0,'')`, value, value); err != nil {
			t.Fatal(err)
		}
	}

	// A new Service has no in-memory scheduler state. It must derive the
	// reconciliation decision entirely from durable sync history.
	restarted := &Service{Store: db, now: func() time.Time { return now }}
	if got := restarted.nextKind(ctx, store.Account{ID: "a-restart", ReconciliationIntervalMinutes: 1440}); got != "reconciliation" {
		t.Fatalf("next kind after incremental history=%q, want reconciliation", got)
	}
}

func TestReconcileIndexedMessagesRemovesRemoteDeletionsWithinMaildir(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('u','u','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a','u','Work','work@example.com','Work','imap.example','user','sealed','smtp.example','user','sealed','{}','now')`); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "mail", "a")
	if err := os.MkdirAll(filepath.Join(root, "Inbox", "new"), 0700); err != nil {
		t.Fatal(err)
	}
	keepPath := filepath.Join(root, "Inbox", "new", "keep:2,")
	gonePath := filepath.Join(root, "Inbox", "new", "gone:2,")
	for _, path := range []string{keepPath, gonePath} {
		if err := os.WriteFile(path, []byte("mail"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []struct{ path, messageID string }{{keepPath, "<keep@example>"}, {gonePath, "<gone@example>"}} {
		if _, err := db.DB.ExecContext(ctx, `INSERT INTO mail_messages(account_id,folder,path,message_id,updated_at) VALUES(?,?,?,?,?)`, "a", "Inbox", item.path, item.messageID, "now"); err != nil {
			t.Fatal(err)
		}
	}
	service := &Service{Store: db, Root: filepath.Dir(filepath.Dir(root))}
	remote := map[string]struct{}{remoteIdentityKey("Inbox", remoteimap.MessageIdentity{MessageID: "<keep@example>"}): {}}
	if err := service.reconcileIndexedMessages(ctx, store.Account{ID: "a"}, remote); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(gonePath); !os.IsNotExist(err) {
		t.Fatalf("remote deletion left local file: %v", err)
	}
	if _, err := os.Stat(keepPath); err != nil {
		t.Fatalf("remote message was removed unexpectedly: %v", err)
	}
	var count int
	if err := db.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM mail_messages WHERE account_id='a'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("indexed message count=%d, want 1", count)
	}
}

func TestRemoteIdentityPrefersUIDOverDuplicateMessageID(t *testing.T) {
	first := remoteimap.MessageIdentity{UID: 10, UIDValidity: 7, MessageID: "<same@example>"}
	second := remoteimap.MessageIdentity{UID: 11, UIDValidity: 7, MessageID: "<same@example>"}
	if remoteIdentityKey("Inbox", first) == remoteIdentityKey("Inbox", second) {
		t.Fatal("different UID identities collapsed to one key")
	}
	if remoteIdentityKey("Inbox", remoteimap.MessageIdentity{MessageID: "<same@example>"}) != remoteMessageIDKey("Inbox", "<same@example>") {
		t.Fatal("message-id fallback key changed")
	}
}

func TestReconcileUsesMbsyncFarIdentityAndQuarantinesStaleMail(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('u-state','state-user','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a-state','u-state','Work','work@example.com','Work','imap.example','user','sealed','smtp.example','user','sealed','{}','now')`); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "mail", "a-state")
	inbox := filepath.Join(root, "Inbox")
	for _, dir := range []string{filepath.Join(inbox, "cur"), filepath.Join(inbox, "new"), filepath.Join(inbox, "tmp")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(inbox, ".mbsyncstate"), []byte("FarUidValidity 77\nMaxPulledUid 124\nNearUidValidity 88\nMaxExpiredFarUid 0\nMaxPushedUid 0\n\n123 456 S\n124 457\n"), 0600); err != nil {
		t.Fatal(err)
	}
	keepPath := filepath.Join(inbox, "cur", "0.1_keep,U=456:2,S")
	stalePath := filepath.Join(inbox, "new", "0.1_stale,U=457:2,")
	for _, path := range []string{keepPath, stalePath} {
		if err := os.WriteFile(path, []byte("mail"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{keepPath, stalePath} {
		if _, err := db.DB.ExecContext(ctx, `INSERT INTO mail_messages(account_id,folder,path,updated_at) VALUES('a-state','Inbox',?,'now')`, path); err != nil {
			t.Fatal(err)
		}
	}
	quarantineRoot := filepath.Join(t.TempDir(), "quarantine")
	service := &Service{Store: db, Root: filepath.Dir(filepath.Dir(root)), QuarantineRoot: quarantineRoot}
	remote := map[string]struct{}{remoteIdentityKey("Inbox", remoteimap.MessageIdentity{UID: 123, UIDValidity: 77}): {}}
	if err := service.reconcileIndexedMessages(ctx, store.Account{ID: "a-state", UserID: "u-state"}, remote); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keepPath); err != nil {
		t.Fatalf("state-mapped message was not retained: %v", err)
	}
	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Fatalf("stale message remained in Maildir: %v", err)
	}
	quarantined := false
	if err := filepath.Walk(quarantineRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && filepath.Base(path) == filepath.Base(stalePath) {
			quarantined = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !quarantined {
		t.Fatal("stale message was not quarantined")
	}
	var uid, validity uint32
	if err := db.DB.QueryRowContext(ctx, `SELECT imap_uid,uid_validity FROM mail_messages WHERE path=?`, keepPath).Scan(&uid, &validity); err != nil {
		t.Fatal(err)
	}
	if uid != 123 || validity != 77 {
		t.Fatalf("indexed identity=%d/%d, want far UID 123/77", uid, validity)
	}
	var decisions int
	if err := db.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log WHERE action='mail_reconciliation_cleanup'`).Scan(&decisions); err != nil {
		t.Fatal(err)
	}
	if decisions != 1 {
		t.Fatalf("cleanup decisions=%d, want 1", decisions)
	}
}

func TestParseMbsyncStateRejectsAmbiguousMappings(t *testing.T) {
	valid := []byte("FarUidValidity 7\nNearUidValidity 8\nMaxPulledUid 2\n\n11 21 S\n12 22\n")
	state, err := parseMBSyncFolderState(valid, "state")
	if err != nil {
		t.Fatal(err)
	}
	if !state.available || state.farUIDValidity != 7 || state.nearToFar[21] != 11 {
		t.Fatalf("parsed state=%#v", state)
	}
	for _, malformed := range [][]byte{
		[]byte("FarUidValidity 7\nNearUidValidity 8\n\n11 21\n11 22\n"),
		[]byte("FarUidValidity 7\nNearUidValidity 8\n\n11 21\n12 21\n"),
		[]byte("FarUidValidity 7\nNearUidValidity 8\n\nnot-a-mapping\n"),
	} {
		if _, err := parseMBSyncFolderState(malformed, "malformed-state"); err == nil {
			t.Fatal("ambiguous or malformed mbsync state was accepted")
		}
	}
}

func TestReconcileDoesNotDeleteUnknownOrSymlinkedOutsideEntries(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('u','u','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a','u','Work','work@example.com','Work','imap.example','user','sealed','smtp.example','user','sealed','{}','now')`); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "mail", "a")
	inside := filepath.Join(root, "Inbox", "new")
	if err := os.MkdirAll(inside, 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside-message")
	if err := os.WriteFile(outside, []byte("do not remove"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(inside, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO mail_messages(account_id,folder,path,message_id,updated_at) VALUES('a','Inbox',?,'<outside@example>','now')`, link); err != nil {
		t.Fatal(err)
	}
	unknown := filepath.Join(inside, "unknown")
	if err := os.WriteFile(unknown, []byte("unknown"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO mail_messages(account_id,folder,path,updated_at) VALUES('a','Inbox',?,'now')`, unknown); err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: db, Root: filepath.Dir(filepath.Dir(root))}
	if err := service.reconcileIndexedMessages(ctx, store.Account{ID: "a"}, map[string]struct{}{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("outside symlink target changed: %v", err)
	}
	var count int
	if err := db.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM mail_messages WHERE account_id='a'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("identity-unknown/symlink rows were removed: count=%d", count)
	}
}
