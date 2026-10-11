package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/voxmail/voxmail/internal/folderpolicy"
)

func TestFolderConfigurationAtomicChoicesAndRollback(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.DB.Exec(`INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('owner','owner','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	original := map[string]string{"INBOX": "Inbox", "[Gmail]/Inbox": "GmailInbox"}
	raw, _ := json.Marshal(original)
	if _, err := db.DB.Exec(`INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a','owner','Mail','a@e','A','h','u','p','h','u','p',?,'now')`, string(raw)); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveFolderRoles(ctx, "owner", "a", map[string]string{"inbox": "INBOX"}, original); err != nil {
		t.Fatal(err)
	}
	mapping := map[string]string{"INBOX": "Inbox", "[Gmail]/Inbox": "SelectedInbox"}
	roles := map[string]string{"inbox": "[Gmail]/Inbox"}
	policy := folderpolicy.Policy{Groups: []folderpolicy.Group{{ID: "chosen", Name: "My inbox", Primary: "[Gmail]/Inbox", Folders: []string{"INBOX", "[Gmail]/Inbox"}}}}
	assertOriginal := func() {
		t.Helper()
		var saved string
		var version int
		if err := db.DB.QueryRow(`SELECT folder_map,config_version FROM accounts WHERE id='a'`).Scan(&saved, &version); err != nil {
			t.Fatal(err)
		}
		var parsed map[string]string
		if err := json.Unmarshal([]byte(saved), &parsed); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(parsed, original) || version != 1 {
			t.Fatalf("partial mapping/version: %s %d", saved, version)
		}
		savedRoles, err := db.ListFolderRoles(ctx, "owner", "a")
		if err != nil || len(savedRoles) != 1 || savedRoles[0].RemotePath != "INBOX" || savedRoles[0].LocalPath != "Inbox" {
			t.Fatalf("partial roles: %+v %v", savedRoles, err)
		}
		savedPolicy, err := db.FolderPolicy(ctx, "owner", "a")
		if err != nil || len(savedPolicy.Groups) != 0 || len(savedPolicy.Excluded) != 0 {
			t.Fatalf("partial policy: %+v %v", savedPolicy, err)
		}
	}
	for _, choices := range []struct{ mapping, roles map[string]string }{{nil, roles}, {mapping, nil}} {
		if err := db.SaveFolderConfiguration(ctx, "owner", "a", 1, policy, choices.mapping, choices.roles); !errors.Is(err, ErrInvalidAccountConfig) {
			t.Fatalf("asymmetric metadata accepted: %v", err)
		}
		assertOriginal()
	}
	// Policy validation follows the mapping/role writes; its failure must roll both back.
	if err := db.SaveFolderConfiguration(ctx, "owner", "a", 1, folderpolicy.Policy{Excluded: []string{"Unknown"}}, mapping, roles); !errors.Is(err, ErrInvalidAccountConfig) {
		t.Fatalf("invalid policy: %v", err)
	}
	assertOriginal()
	injected := errors.New("role insert failed")
	db.folderRoleInsertHook = func() error { return injected }
	if err := db.SaveFolderConfiguration(ctx, "owner", "a", 1, policy, mapping, roles); !errors.Is(err, injected) {
		t.Fatalf("injected failure: %v", err)
	}
	db.folderRoleInsertHook = nil
	assertOriginal()
	if err := db.SaveFolderConfiguration(ctx, "other", "a", 1, policy, mapping, roles); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("cross-owner save: %v", err)
	}
	assertOriginal()
	if err := db.SaveFolderConfiguration(ctx, "owner", "a", 1, policy, mapping, roles); err != nil {
		t.Fatal(err)
	}
	savedPolicy, version, err := db.FolderPolicySnapshot(ctx, "owner", "a")
	if err != nil || version != 2 || !reflect.DeepEqual(savedPolicy, policy) {
		t.Fatalf("combined save: %+v %d %v", savedPolicy, version, err)
	}
	savedRoles, err := db.ListFolderRoles(ctx, "owner", "a")
	if err != nil || len(savedRoles) != 1 || savedRoles[0].RemotePath != "[Gmail]/Inbox" || savedRoles[0].LocalPath != "SelectedInbox" {
		t.Fatalf("saved roles: %+v %v", savedRoles, err)
	}
	var saved string
	if err := db.DB.QueryRow(`SELECT folder_map FROM accounts WHERE id='a'`).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	var parsed map[string]string
	if err := json.Unmarshal([]byte(saved), &parsed); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed, mapping) {
		t.Fatalf("mapping missing from commit: %s", saved)
	}
	if err := db.SaveFolderConfiguration(ctx, "owner", "a", 1, folderpolicy.Policy{}, original, map[string]string{}); !errors.Is(err, ErrAccountConfigConflict) {
		t.Fatalf("stale save: %v", err)
	}
	savedPolicy, version, err = db.FolderPolicySnapshot(ctx, "owner", "a")
	if err != nil || version != 2 || !reflect.DeepEqual(savedPolicy, policy) {
		t.Fatalf("stale save changed choices: %+v %d %v", savedPolicy, version, err)
	}
}

func TestFolderConfigurationRetainsCaseDistinctRemoteRoles(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "case.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.DB.Exec(`INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('owner','owner','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	mapping := map[string]string{"Sent": "UpperSent", "sent": "LowerSent"}
	raw, _ := json.Marshal(mapping)
	if _, err := db.DB.Exec(`INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a','owner','Mail','a@e','A','h','u','p','h','u','p',?,'now')`, string(raw)); err != nil {
		t.Fatal(err)
	}
	account := Account{ID: "a", UserID: "owner", FolderMap: string(raw)}
	if _, err := db.FolderRole(ctx, account, "sent"); !errors.Is(err, ErrInvalidFolderRole) {
		t.Fatalf("ambiguous legacy role guessed: %v", err)
	}
	roles := map[string]string{"sent": "Sent", "drafts": "sent"}
	if err := db.SaveFolderConfiguration(ctx, "owner", "a", 1, folderpolicy.Policy{}, mapping, roles); err != nil {
		t.Fatal(err)
	}
	for role, remote := range roles {
		saved, err := db.FolderRole(ctx, account, role)
		if err != nil || saved.RemotePath != remote || saved.LocalPath != mapping[remote] {
			t.Fatalf("case-distinct role collapsed: %+v %v", saved, err)
		}
	}
	mailboxes, err := db.ListLogicalMailboxes(ctx, "owner", "a")
	if err != nil || len(mailboxes) != 2 {
		t.Fatalf("case-distinct folders hidden: %+v %v", mailboxes, err)
	}
	policy := folderpolicy.Policy{Groups: []folderpolicy.Group{{ID: "explicit", Name: "My sent", Primary: "sent", Folders: []string{"Sent", "sent"}}}}
	if err := db.SaveFolderConfiguration(ctx, "owner", "a", 2, policy, mapping, roles); err != nil {
		t.Fatal(err)
	}
	remote, local, err := db.GroupDestination(ctx, "owner", "a", "/groups/explicit")
	if err != nil || remote != "sent" || local != "LowerSent" {
		t.Fatalf("primary changed case identity: %q %q %v", remote, local, err)
	}
}

func TestFolderMappingsNormalizeOnlyReservedInboxIdentity(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		valid bool
	}{
		{`{"Sent":"UpperSent","sent":"LowerSent"}`, true},
		{`{"INBOX":"Inbox","inbox":"OtherInbox"}`, false},
		{`{"One":"Same","Two":"same"}`, false},
	} {
		_, err := validateAccountFolderMap(tc.raw)
		if (err == nil) != tc.valid {
			t.Fatalf("mapping %s: %v", tc.raw, err)
		}
	}
	if _, err := validateFolderRoleMappings(map[string]string{"sent": "SENT"}, map[string]string{"Sent": "UpperSent"}); !errors.Is(err, ErrInvalidFolderRole) {
		t.Fatalf("role silently matched different remote case: %v", err)
	}
}
