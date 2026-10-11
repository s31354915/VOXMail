package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/voxmail/voxmail/internal/folderpolicy"
)

func TestRemapRetainsSourceIdentityAndReservesHistoryAcrossRestartAndPrune(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "bindings.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { db.Close() }()
	if _, err := db.DB.Exec(`INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('owner','owner','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a','owner','Mail','a@e','A','h','u','p','h','u','p','{"INBOX":"Inbox","Alias":"Before","Archive":"Archive"}','now')`); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertRemoteFolder(ctx, "a", "Alias", "Before", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO mail_messages(account_id,folder,path,message_id,imap_uid,uid_validity,updated_at) VALUES('a','Before','/mail/Before','<same@test>',7,101,'now')`); err != nil {
		t.Fatal(err)
	}
	mapping := map[string]string{"INBOX": "Inbox", "Alias": "After", "Archive": "Archive"}
	policy := folderpolicy.Policy{Groups: []folderpolicy.Group{{ID: "inbox", Name: "My inbox", Primary: "Alias", Folders: []string{"INBOX", "Alias"}}}}
	if err := db.SaveFolderConfiguration(ctx, "owner", "a", 1, policy, mapping, map[string]string{"inbox": "Alias"}); err != nil {
		t.Fatal(err)
	}
	inbox, err := db.InboxFolder(ctx, Account{ID: "a", UserID: "owner"})
	if err != nil || inbox != "Before" {
		t.Fatalf("pending role remap lost observed Inbox: %q %v", inbox, err)
	}
	if err := db.UpsertRemoteFolder(ctx, "a", "Alias", "After", ""); err != nil {
		t.Fatal(err)
	}
	for _, local := range []string{"Before", "After"} {
		remote, err := db.RemoteFolderForLocal(ctx, "owner", "a", local)
		if err != nil || remote != "Alias" {
			t.Fatalf("remap guessed source for %s: %q %v", local, remote, err)
		}
	}
	var folder string
	var uid, validity int
	if err := db.DB.QueryRow(`SELECT folder,imap_uid,uid_validity FROM mail_messages WHERE account_id='a'`).Scan(&folder, &uid, &validity); err != nil || folder != "Before" || uid != 7 || validity != 101 {
		t.Fatalf("observation rewrote message identity: %s %d %d %v", folder, uid, validity, err)
	}
	if _, err := db.DB.Exec(`INSERT INTO mail_messages(account_id,folder,path,message_id,imap_uid,uid_validity,updated_at) VALUES('a','After','/mail/After','<same@test>',8,102,'now')`); err != nil {
		t.Fatal(err)
	}
	grouped, err := db.ListMailForAccount(ctx, "owner", "a", "/groups/inbox", false)
	if err != nil || len(grouped) != 2 || grouped[0].ID == grouped[1].ID || grouped[0].UIDValidity == grouped[1].UIDValidity {
		t.Fatalf("partial reconciliation lost or collapsed grouped identities: %+v %v", grouped, err)
	}
	unread, err := db.ListMail(ctx, "owner", true)
	if err != nil || len(unread) != 2 {
		t.Fatalf("remapped Inbox lost unread historical members: %+v %v", unread, err)
	}
	mailboxes, err := db.ListLogicalMailboxes(ctx, "owner", "a")
	if err != nil || len(mailboxes) != 2 {
		t.Fatalf("historical aliases appeared as extra mailboxes: %+v %v", mailboxes, err)
	}
	for _, mailbox := range mailboxes {
		if mailbox.Key == "Before" || mailbox.Key == "After" {
			t.Fatalf("retained member listed separately: %+v", mailbox)
		}
	}
	if err := db.SaveFolderPolicy(ctx, "owner", "a", 2, folderpolicy.Policy{}); err != nil {
		t.Fatal(err)
	}
	mailboxes, err = db.ListLogicalMailboxes(ctx, "owner", "a")
	if err != nil || len(mailboxes) != 3 {
		t.Fatalf("separating provider folders exposed historical directories: %+v %v", mailboxes, err)
	}
	separate, err := db.ListMailForAccount(ctx, "owner", "a", "After", false)
	if err != nil || len(separate) != 2 {
		t.Fatalf("ungrouped remote lost its old indexed directory: %+v %v", separate, err)
	}
	if _, err := db.RemoteFolderForLocal(ctx, "other", "a", "Before"); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("cross-owner history access: %v", err)
	}
	if _, err := db.RemoteFolderForLocal(ctx, "owner", "a", "UnknownAlias"); !errors.Is(err, ErrInvalidAccountConfig) {
		t.Fatalf("unbound alias guessed as remote: %v", err)
	}
	// A failed observation must roll back both the new binding and metadata.
	if _, err := db.DB.Exec(`CREATE TRIGGER reject_observation BEFORE INSERT ON remote_folders WHEN NEW.local_path='Failure' BEGIN SELECT RAISE(ABORT,'injected observation failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertRemoteFolder(ctx, "a", "Alias", "Failure", ""); err == nil {
		t.Fatal("injected observation failure ignored")
	}
	if _, err := db.RemoteFolderForLocal(ctx, "owner", "a", "Failure"); !errors.Is(err, ErrInvalidAccountConfig) {
		t.Fatalf("failed observation retained history: %v", err)
	}
	if err := db.PruneRemoteFolders(ctx, "a", nil); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := db.RemoteFolderForLocal(ctx, "owner", "a", "Before")
	if err != nil || remote != "Alias" {
		t.Fatalf("prune/restart forgot retained source: %q %v", remote, err)
	}
	mapping["Archive"] = "Before"
	if err := db.SaveFolderConfiguration(ctx, "owner", "a", 3, policy, mapping, map[string]string{"inbox": "INBOX"}); !errors.Is(err, ErrInvalidAccountConfig) {
		t.Fatalf("historical directory reused: %v", err)
	}
	if err := db.SaveFolderPolicy(ctx, "owner", "a", 3, folderpolicy.Policy{Excluded: []string{"Alias"}}); err != nil {
		t.Fatal(err)
	}
	visible, err := db.ListMail(ctx, "owner", false)
	if err != nil || len(visible) != 0 {
		t.Fatalf("exclusion left historical remap copies visible: %+v %v", visible, err)
	}
	// Imported incompatible observations remain ambiguous instead of overwriting history.
	if err := db.UpsertRemoteFolder(ctx, "a", "DifferentRemote", "Before", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RemoteFolderForLocal(ctx, "owner", "a", "Before"); !errors.Is(err, ErrInvalidAccountConfig) {
		t.Fatalf("ambiguous historical binding guessed: %v", err)
	}
	if _, err := db.DB.Exec(`DELETE FROM accounts WHERE id='a'`); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM account_folder_bindings`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("deleted account retained bindings: %d %v", count, err)
	}
}

func TestFolderBindingMigrationBackfillsExistingObservations(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "upgrade.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('owner','owner','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a','owner','Mail','a@e','A','h','u','p','h','u','p','{}','now')`); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertRemoteFolder(ctx, "a", "Alias", "Original", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`DROP TABLE account_folder_bindings`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`DELETE FROM schema_migrations WHERE version=25`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.UpsertRemoteFolder(ctx, "a", "Alias", "Replacement", ""); err != nil {
		t.Fatal(err)
	}
	remote, err := db.RemoteFolderForLocal(ctx, "owner", "a", "Original")
	if err != nil || remote != "Alias" {
		t.Fatalf("upgrade failed to preserve observation: %q %v", remote, err)
	}
}

func TestSyncBindingsPreservePartialImportSourceAfterMappingChanges(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "partial-bindings.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.DB.Exec(`INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('owner','owner','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a','owner','Mail','a@e','A','h','u','p','h','u','p','{"Alias":"Before"}','now')`); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordFolderDiscovery(ctx, "owner", "a", 1, []string{"Unmapped"}); err != nil {
		t.Fatal(err)
	}
	if err := db.BindSyncFolders(ctx, "other", "a", 1); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("cross-owner sync bindings: %v", err)
	}
	if err := db.BindSyncFolders(ctx, "owner", "a", 1); err != nil {
		t.Fatal(err)
	}
	// Configuration associations are durable even if the runner later times out
	// before an authenticated reconciliation can observe the folders.
	mapping := map[string]string{"Alias": "After"}
	if err := db.SaveFolderConfiguration(ctx, "owner", "a", 1, folderpolicy.Policy{}, mapping, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	for local, expected := range map[string]string{"Before": "Alias", "Inbox": "INBOX", "Unmapped": "Unmapped"} {
		remote, err := db.RemoteFolderForLocal(ctx, "owner", "a", local)
		if err != nil || remote != expected {
			t.Fatalf("partial source %s: %q %v", local, remote, err)
		}
	}
	if err := db.BindSyncFolders(ctx, "owner", "a", 1); !errors.Is(err, ErrAccountConfigConflict) {
		t.Fatalf("stale sync configuration accepted: %v", err)
	}
	var count int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM account_folder_bindings WHERE local_path='After'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("stale sync partially bound current path: %d %v", count, err)
	}
	if err := db.BindSyncFolders(ctx, "owner", "a", 2); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := db.DB.QueryRow(`SELECT config_version FROM accounts WHERE id='a'`).Scan(&version); err != nil || version != 2 {
		t.Fatalf("binding advanced owner configuration: %d %v", version, err)
	}
	var observations int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM remote_folders WHERE account_id='a'`).Scan(&observations); err != nil || observations != 0 {
		t.Fatalf("planned sync fabricated observations: %d %v", observations, err)
	}
}
