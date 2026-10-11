package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/voxmail/voxmail/internal/folderpolicy"
)

func TestFolderDiscoveryOffersMappingBeforeSyncWithoutReplacingPhysicalIdentity(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "discovery.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.DB.Exec(`INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('owner','owner','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a','owner','Mail','a@e','A','h','u','p','h','u','p','{"INBOX":"PendingInbox"}','now')`); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertRemoteFolder(ctx, "a", "INBOX", "ObservedInbox", "inbox"); err != nil {
		t.Fatal(err)
	}
	folders := []string{"INBOX", "[Gmail]/Inbox", "Sent", "sent"}
	if err := db.RecordFolderDiscovery(ctx, "other", "a", 1, folders); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("cross-owner discovery: %v", err)
	}
	if err := db.RecordFolderDiscovery(ctx, "owner", "a", 2, folders); !errors.Is(err, ErrAccountConfigConflict) {
		t.Fatalf("stale credentials discovery: %v", err)
	}
	if err := db.RecordFolderDiscovery(ctx, "owner", "a", 1, []string{"ShouldNotPersist", "../Unsafe"}); !errors.Is(err, ErrInvalidAccountConfig) {
		t.Fatalf("unsafe discovery: %v", err)
	}
	var count int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM account_folder_discovery`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed discovery partially retained names: %d %v", count, err)
	}
	if err := db.RecordFolderDiscovery(ctx, "owner", "a", 1, folders); err != nil {
		t.Fatal(err)
	}
	inventory, err := db.FolderInventory(ctx, "owner", "a")
	if err != nil || len(inventory) != 4 || inventory["INBOX"] != "ObservedInbox" || inventory["Sent"] != "Sent" || inventory["sent"] != "sent" {
		t.Fatalf("discovery changed physical identities: %v %v", inventory, err)
	}
	var physical, role string
	if err := db.DB.QueryRow(`SELECT local_path,role FROM remote_folders WHERE account_id='a' AND remote_path='INBOX'`).Scan(&physical, &role); err != nil || physical != "ObservedInbox" || role != "inbox" {
		t.Fatalf("provider LIST rewrote synchronization metadata: %s %s %v", physical, role, err)
	}
	mapping := map[string]string{"INBOX": "PendingInbox", "[Gmail]/Inbox": "AliasInbox", "Sent": "UpperSent", "sent": "LowerSent"}
	policy := folderpolicy.Policy{Groups: []folderpolicy.Group{{ID: "selected", Name: "My inbox", Primary: "[Gmail]/Inbox", Folders: []string{"INBOX", "[Gmail]/Inbox"}}}}
	if err := db.SaveFolderConfiguration(ctx, "owner", "a", 1, policy, mapping, map[string]string{"inbox": "INBOX", "sent": "sent"}); err != nil {
		t.Fatalf("mapping before sync rejected discovered folder: %v", err)
	}
	inventory, err = db.FolderInventory(ctx, "owner", "a")
	if err != nil || inventory["INBOX"] != "ObservedInbox" || inventory["[Gmail]/Inbox"] != "AliasInbox" || inventory["sent"] != "LowerSent" {
		t.Fatalf("observed and pending paths confused: %v %v", inventory, err)
	}
	if err := db.RecordFolderDiscovery(ctx, "owner", "a", 1, []string{"OldSettingsFolder"}); !errors.Is(err, ErrAccountConfigConflict) {
		t.Fatalf("old discovery survived mapping change: %v", err)
	}
	if _, err := db.DB.Exec(`DELETE FROM accounts WHERE id='a'`); err != nil {
		t.Fatal(err)
	}
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM account_folder_discovery`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("discovery survived account deletion: %d %v", count, err)
	}
}
