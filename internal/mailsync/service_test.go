package mailsync

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	remoteimap "github.com/voxmail/voxmail/internal/imap"
	"github.com/voxmail/voxmail/internal/store"
)

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
