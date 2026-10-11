package calls

import (
	"context"
	"strings"
	"testing"

	"github.com/voxmail/voxmail/internal/folderpolicy"
	"github.com/voxmail/voxmail/internal/store"
)

func TestPhoneGroupsKeepStandardKeysAndDistinctMessageIdentities(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()
	if err := db.CreateUser(ctx, store.User{ID: "owner", Username: "owner", PasswordHash: "p", PINHash: "p", Role: "user", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	mapping := `{"INBOX":"Inbox","[Gmail]/Inbox":"GmailInbox","Drafts":"Drafts","Sent":"Sent","Trash":"Trash","Archive":"Archive","OldArchive":"OldArchive","Hidden":"Hidden"}`
	if _, err := db.DB.Exec(`INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a','owner','Mail','a@e','A','h','u','p','h','u','p',?,'now')`, mapping); err != nil {
		t.Fatal(err)
	}
	account := store.Account{ID: "a", UserID: "owner", CanonicalName: "Mail", FolderMap: mapping}
	policy := folderpolicy.Policy{Groups: []folderpolicy.Group{
		{ID: "inbox-pair", Name: "Inbox", Primary: "[Gmail]/Inbox", Folders: []string{"INBOX", "[Gmail]/Inbox"}},
		{ID: "saved", Name: "Saved mail", Primary: "OldArchive", Folders: []string{"Archive", "OldArchive"}},
	}, Excluded: []string{"Hidden"}}
	if err := db.SaveFolderPolicy(ctx, "owner", "a", 1, policy); err != nil {
		t.Fatal(err)
	}
	for i, folder := range []string{"Inbox", "GmailInbox"} {
		if _, err := db.DB.Exec(`INSERT INTO mail_messages(account_id,folder,path,message_id,imap_uid,uid_validity,updated_at) VALUES('a',?,?, '<same@test>',7,?,'now')`, folder, "/mail/"+folder, i+100); err != nil {
			t.Fatal(err)
		}
	}
	sess := &session{CallID: "group-test", UserID: "owner", ActiveAccount: "a", State: "account_menu", Accounts: []store.Account{account}}
	svc.openAccountFolders(sess, account)
	if sess.State != "folders" || len(sess.Folders) != 5 {
		t.Fatalf("logical menu=%q %v", sess.State, sess.Folders)
	}
	if sess.FolderRolePaths["inbox"] != "/groups/inbox-pair" || sess.FolderRolePaths["drafts"] != "Drafts" || sess.FolderRolePaths["sent"] != "Sent" || sess.FolderRolePaths["trash"] != "Trash" {
		t.Fatalf("fixed roles lost: %v", sess.FolderRolePaths)
	}
	prompt := folderPromptFor(account, sess.Folders, sess.FolderRolePaths, "", 0, sess.FolderLabels)
	for _, expected := range []string{"Press 1 for Inbox", "Press 2 for Drafts", "Press 3 for Sent", "Press 4 for Trash", "Press 5 for Saved mail"} {
		if !strings.Contains(prompt, expected) {
			t.Fatalf("missing %q in %s", expected, prompt)
		}
	}
	if strings.Contains(prompt, "/groups/") || strings.Contains(prompt, "Hidden") || strings.Contains(prompt, "GmailInbox") {
		t.Fatalf("implementation identity/aliases leaked in menu: %s", prompt)
	}
	svc.handleFolderMenu(sess, "1")
	if sess.SelectedFolder != "/groups/inbox-pair" || len(sess.Messages) != 2 || sess.Messages[0].ID == sess.Messages[1].ID || sess.Messages[0].UIDValidity == sess.Messages[1].UIDValidity {
		t.Fatalf("group selection collapsed identities: %+v", sess.Messages)
	}
	svc.openAccountFolders(sess, account)
	svc.handleFolderMenu(sess, "5")
	if sess.SelectedFolder != "/groups/saved" || sess.State != "folder_action" {
		t.Fatalf("group parsed as synthetic hierarchy: %s %s", sess.SelectedFolder, sess.State)
	}
	if children := folderChildren(sess.Folders, "/groups/saved"); len(children) != 0 {
		t.Fatalf("group has synthetic nested paths: %v", children)
	}
}

func TestPhoneGroupKeysStayAtomicAndCaseDistinct(t *testing.T) {
	folders := []string{"/groups/A", "/groups/a", "Archive/Projects"}
	children := folderMenuChildren(folders, map[string]string{"inbox": "/groups/A"}, "")
	if len(children) != 2 || children[0] != "/groups/a" || children[1] != "Archive" {
		t.Fatalf("keys conflated or split: %v", children)
	}
}

func TestFolderMenuKeepsSpamAndArchiveRolesInOtherFolders(t *testing.T) {
	folders := []string{"Inbox", "Drafts", "Sent", "Trash", "Spam", "Archive"}
	roles := map[string]string{"inbox": "Inbox", "drafts": "Drafts", "sent": "Sent", "trash": "Trash", "spam": "Spam", "archive": "Archive"}
	children := folderMenuChildren(folders, roles, "")
	if len(children) != 2 || children[0] != "Archive" || children[1] != "Spam" {
		t.Fatalf("non-fixed roles disappeared from browsing: %v", children)
	}
}

func TestPhoneLegacyRolesRespectAliasesAndRequireChoiceForAmbiguousRemotes(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()
	if err := db.CreateUser(ctx, store.User{ID: "owner", Username: "owner", PasswordHash: "p", PINHash: "p", Role: "user", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	mapping := `{"INBOX":"MailInbox","Sent":"UpperSent","Drafts":"MyDrafts"}`
	if _, err := db.DB.Exec(`INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a','owner','Mail','a@e','A','h','u','p','h','u','p',?,'now')`, mapping); err != nil {
		t.Fatal(err)
	}
	account := store.Account{ID: "a", UserID: "owner", CanonicalName: "Mail", FolderMap: mapping}
	sess := &session{CallID: "legacy", UserID: "owner", ActiveAccount: "a", State: "account_menu", Accounts: []store.Account{account}}
	svc.openAccountFolders(sess, account)
	if sess.FolderRolePaths["sent"] != "UpperSent" || sess.FolderRolePaths["drafts"] != "MyDrafts" {
		t.Fatalf("conventional remote mapping lost fixed role: %v", sess.FolderRolePaths)
	}
	account.FolderMap = `{"INBOX":"MailInbox","Sent":"UpperSent","sent":"LowerSent","Drafts":"MyDrafts"}`
	if _, err := db.DB.Exec(`UPDATE accounts SET folder_map=? WHERE id='a'`, account.FolderMap); err != nil {
		t.Fatal(err)
	}
	svc.openAccountFolders(sess, account)
	if sess.FolderRolePaths["sent"] != "" {
		t.Fatalf("ambiguous remote role guessed: %v", sess.FolderRolePaths)
	}
	children := folderMenuChildren(sess.Folders, sess.FolderRolePaths, "")
	if len(children) != 2 || children[0] != "LowerSent" || children[1] != "UpperSent" {
		t.Fatalf("distinct unassigned remotes unavailable: %v", children)
	}
	if err := db.SaveFolderRoles(ctx, "owner", "a", map[string]string{"sent": "sent"}, map[string]string{"INBOX": "MailInbox", "Sent": "UpperSent", "sent": "LowerSent", "Drafts": "MyDrafts"}); err != nil {
		t.Fatal(err)
	}
	svc.openAccountFolders(sess, account)
	if sess.FolderRolePaths["sent"] != "LowerSent" {
		t.Fatalf("owner's exact case choice ignored: %v", sess.FolderRolePaths)
	}
}

func TestPhoneGroupedInboxSurvivesRemapAndKeepsExcludedRoleUnassigned(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()
	if err := db.CreateUser(ctx, store.User{ID: "owner", Username: "owner", PasswordHash: "p", PINHash: "p", Role: "user", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	mapping := `{"INBOX":"Inbox","Alias":"Before","Drafts":"Drafts","AltDraft":"AltDraft"}`
	if _, err := db.DB.Exec(`INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a','owner','Mail','a@e','A','h','u','p','h','u','p',?,'now')`, mapping); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertRemoteFolder(ctx, "a", "Alias", "Before", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO mail_messages(account_id,folder,path,message_id,imap_uid,uid_validity,updated_at) VALUES('a','Before','/mail/Before','<same@test>',7,101,'now')`); err != nil {
		t.Fatal(err)
	}
	policy := folderpolicy.Policy{Groups: []folderpolicy.Group{{ID: "inbox", Name: "My inbox", Primary: "Alias", Folders: []string{"INBOX", "Alias"}}}, Excluded: []string{"AltDraft"}}
	if err := db.SaveFolderConfiguration(ctx, "owner", "a", 1, policy, map[string]string{"INBOX": "Inbox", "Alias": "After", "Drafts": "Drafts", "AltDraft": "AltDraft"}, map[string]string{"inbox": "Alias", "drafts": "AltDraft"}); err != nil {
		t.Fatal(err)
	}
	accounts, err := db.ListAccounts(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	account := accounts[0]
	sess := &session{CallID: "remap", UserID: "owner", ActiveAccount: "a", State: "account_menu", Accounts: accounts}
	svc.openAccountFolders(sess, account)
	if sess.FolderRolePaths["inbox"] != "/groups/inbox" || sess.FolderRolePaths["drafts"] != "" {
		t.Fatalf("pending mapping changed role choices: %v", sess.FolderRolePaths)
	}
	if err := db.UpsertRemoteFolder(ctx, "a", "Alias", "After", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO mail_messages(account_id,folder,path,message_id,imap_uid,uid_validity,updated_at) VALUES('a','After','/mail/After','<same@test>',8,102,'now')`); err != nil {
		t.Fatal(err)
	}
	svc.openAccountFolders(sess, account)
	if len(sess.Folders) != 2 {
		t.Fatalf("historical physical aliases exposed as extra folders: %v", sess.Folders)
	}
	svc.handleFolderMenu(sess, "1")
	if sess.SelectedFolder != "/groups/inbox" || len(sess.Messages) != 2 || sess.Messages[0].ID == sess.Messages[1].ID {
		t.Fatalf("remapped group lost distinct messages: %+v", sess)
	}
}
