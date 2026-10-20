package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/voxmail/voxmail/internal/folderpolicy"
	"github.com/voxmail/voxmail/internal/secret"
)

func TestMappingRejectsOtherRemotePhysicalStateThroughBothSavePaths(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "destinations.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateUser(ctx, User{ID: "owner", Username: "owner", PasswordHash: "p", PINHash: "p", Role: "user", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	box, err := secret.New("test-key-with-more-than-32-characters-123456")
	if err != nil {
		t.Fatal(err)
	}
	original := map[string]string{"INBOX": "Inbox", "Alias": "AliasInbox"}
	raw, _ := json.Marshal(original)
	account := Account{ID: "a", UserID: "owner", CanonicalName: "Mail", Email: "a@e.test", SenderName: "A", IMAPHost: "h", IMAPUser: "u", IMAPPassword: "p", SMTPHost: "h", SMTPUser: "u", SMTPPassword: "p", FolderMap: string(raw)}
	unsafeNew := account
	unsafeNew.FolderMap = `{"Alias":"Inbox"}`
	if err := db.SaveAccount(ctx, box, unsafeNew); !errors.Is(err, ErrInvalidAccountConfig) {
		t.Fatalf("new account reused implicit Inbox state: %v", err)
	}
	if err := db.SaveAccount(ctx, box, account); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertRemoteFolder(ctx, "a", "INBOX", "ObservedInbox", "inbox"); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordFolderDiscovery(ctx, "owner", "a", 1, []string{"Unmapped"}); err != nil {
		t.Fatal(err)
	}
	policy := folderpolicy.Policy{Groups: []folderpolicy.Group{{ID: "pair", Name: "My inbox", Primary: "Alias", Folders: []string{"INBOX", "Alias"}}}}
	for _, target := range []string{"ObservedInbox", "Unmapped", "Inbox"} {
		proposed := map[string]string{"INBOX": "FreshInbox", "Alias": target}
		if err := db.SaveFolderConfiguration(ctx, "owner", "a", 1, policy, proposed, map[string]string{"inbox": "INBOX"}); !errors.Is(err, ErrInvalidAccountConfig) {
			t.Fatalf("logical save accepted other remote's destination %q: %v", target, err)
		}
		data, _ := json.Marshal(proposed)
		account.FolderMap = string(data)
		if err := db.SaveAccount(ctx, box, account); !errors.Is(err, ErrInvalidAccountConfig) {
			t.Fatalf("ordinary account save bypassed destination guard %q: %v", target, err)
		}
	}
	var version int
	var saved string
	if err := db.DB.QueryRow(`SELECT config_version,folder_map FROM accounts WHERE id='a'`).Scan(&version, &saved); err != nil || version != 1 || saved != string(raw) {
		t.Fatalf("collision failures partially saved: %d %s %v", version, saved, err)
	}
	// A fresh directory and an explicit logical group remain valid.
	proposed := map[string]string{"INBOX": "FreshInbox", "Alias": "FreshAlias"}
	if err := db.SaveFolderConfiguration(ctx, "owner", "a", 1, policy, proposed, map[string]string{"inbox": "INBOX"}); err != nil {
		t.Fatal(err)
	}
	remote, err := db.RemoteFolderForLocal(ctx, "owner", "a", "ObservedInbox")
	if err != nil || remote != "INBOX" {
		t.Fatalf("pending remap changed observed identity: %q %v", remote, err)
	}
}

func TestMappingRejectsCanonicalLocalAliasCollisions(t *testing.T) {
	for _, raw := range []string{
		`{"One":"A/B","Two":"A//B"}`,
		`{"One":"A/B","Two":"A/./B"}`,
		`{"One":"A/B","Two":"A\\B"}`,
		`{"One":"."}`,
	} {
		if _, err := validateAccountFolderMap(raw); !errors.Is(err, ErrInvalidAccountConfig) {
			t.Fatalf("unsafe local aliases accepted %s: %v", raw, err)
		}
	}
}

func TestSyncDiscoveryCannotReuseAnOldAliasAsNewRemoteState(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "new-remote.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.DB.Exec(`INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('owner','owner','p','p','user','now'); INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a','owner','Mail','a@e','A','h','u','p','h','u','p','{"Alias":"NewAlias"}','now')`); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertRemoteFolder(ctx, "a", "Alias", "OldAlias", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordFolderDiscovery(ctx, "owner", "a", 1, []string{"INBOX", "Alias", "OldAlias"}); err != nil {
		t.Fatal(err)
	}
	if err := db.BindSyncFolders(ctx, "owner", "a", 1); !errors.Is(err, ErrInvalidAccountConfig) {
		t.Fatalf("new remote reused historical state: %v", err)
	}
	var count int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM account_folder_bindings WHERE account_id='a' AND remote_path='OldAlias'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed sync recorded conflicting identity: %d %v", count, err)
	}
	// The owner can keep both provider folders by choosing a fresh physical
	// alias; no automatic grouping or exclusion is needed.
	if err := db.SaveFolderConfiguration(ctx, "owner", "a", 1, folderpolicy.Policy{}, map[string]string{"Alias": "NewAlias", "OldAlias": "Independent"}, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if err := db.BindSyncFolders(ctx, "owner", "a", 2); err != nil {
		t.Fatalf("owner's independent alias did not resolve collision: %v", err)
	}
}

func TestFolderDestinationCollisionReportsSameFolderEveryRun(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "destinations.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateUser(ctx, User{ID: "owner", Username: "owner", PasswordHash: "p", PINHash: "p", Role: "user", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	// A folder_map that maps two distinct remotes onto one local folder is
	// invalid; the validator must name the same conflict on every sync run
	// instead of whichever map iteration happened to hit first.
	_, err = db.DB.Exec(`INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at)
VALUES('a','owner','Mail','a@e.test','A','h','u','p','h','u','p','{"INBOX":"Inbox","Trash":"Trash","[Gmail]/Trash":"Trash"}','now')`)
	if err != nil {
		t.Fatal(err)
	}
	var first string
	for i := 0; i < 64; i++ {
		err := db.BindSyncFolders(ctx, "owner", "a", 1)
		if err == nil {
			t.Fatal("colliding folder_map must fail destination validation")
		}
		if i == 0 {
			first = err.Error()
			continue
		}
		if err.Error() != first {
			t.Fatalf("collision error changed between runs:\nfirst:  %s\nlater:  %s", first, err.Error())
		}
	}
}
