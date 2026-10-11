package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/voxmail/voxmail/internal/folderpolicy"
)

func TestFolderPolicyPersistenceOwnershipVersionAndPhysicalSeparation(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mail.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { db.Close() }()
	if err := db.CreateUser(ctx, User{ID: "owner", Username: "owner", PasswordHash: "p", PINHash: "p", Role: "user", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	mapping := `{"INBOX":"Inbox","[Gmail]/Inbox":"GmailInbox"}`
	if _, err := db.DB.Exec(`INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('account','owner','Mail','owner@example.test','Owner','imap.test','owner','sealed','smtp.test','owner','sealed',?,'now')`, mapping); err != nil {
		t.Fatal(err)
	}
	policy, err := db.FolderPolicy(ctx, "owner", "account")
	if err != nil || len(policy.Groups) != 0 || len(policy.Excluded) != 0 {
		t.Fatalf("old account changed: %+v %v", policy, err)
	}
	policy = folderpolicy.Policy{Groups: []folderpolicy.Group{{ID: "chosen", Name: "Inbox", Primary: "[Gmail]/Inbox", Folders: []string{"INBOX", "[Gmail]/Inbox"}}}}
	if err := db.SaveFolderPolicy(ctx, "other", "account", 1, policy); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("cross-user save: %v", err)
	}
	if _, err := db.FolderPolicy(ctx, "other", "account"); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("cross-user read: %v", err)
	}
	invalid := folderpolicy.Policy{Excluded: []string{"Unknown"}}
	if err := db.SaveFolderPolicy(ctx, "owner", "account", 1, invalid); !errors.Is(err, ErrInvalidAccountConfig) {
		t.Fatalf("unknown selection: %v", err)
	}
	if err := db.SaveFolderPolicy(ctx, "owner", "account", 1, policy); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveFolderPolicy(ctx, "owner", "account", 1, folderpolicy.Policy{}); !errors.Is(err, ErrAccountConfigConflict) {
		t.Fatalf("stale save: %v", err)
	}
	var savedMapping string
	var version int
	if err := db.DB.QueryRow(`SELECT folder_map,config_version FROM accounts WHERE id='account'`).Scan(&savedMapping, &version); err != nil {
		t.Fatal(err)
	}
	if savedMapping != mapping || version != 2 {
		t.Fatalf("physical aliases/version changed: %s %d", savedMapping, version)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := db.FolderPolicy(ctx, "owner", "account")
	if err != nil || len(loaded.Groups) != 1 || loaded.Groups[0].Primary != "[Gmail]/Inbox" {
		t.Fatalf("owner choice did not survive restart: %+v %v", loaded, err)
	}
	if _, err := db.DB.Exec(`DELETE FROM accounts WHERE id='account'`); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM account_folder_policies`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("deleted account retained policy")
	}
}
