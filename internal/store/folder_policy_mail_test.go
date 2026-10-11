package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/voxmail/voxmail/internal/folderpolicy"
)

func TestGroupedMailPagesRetainDistinctUIDIdentitiesAndExplicitExclusions(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.DB.Exec(`INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('owner','owner','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a','owner','Mail','a@e','A','h','u','p','h','u','p','{"INBOX":"Inbox","[Gmail]/Inbox":"GmailInbox","Archive":"Archive"}','now')`); err != nil {
		t.Fatal(err)
	}
	for i, folder := range []string{"Inbox", "GmailInbox", "Archive"} {
		if _, err := db.DB.Exec(`INSERT INTO mail_messages(account_id,folder,path,message_id,imap_uid,uid_validity,is_read,updated_at) VALUES('a',?,?, '<same@example.test>',7,?,0,'now')`, folder, "/mail/"+folder, i+100); err != nil {
			t.Fatal(err)
		}
	}
	initial, err := db.ListMail(ctx, "owner", false)
	if err != nil || len(initial) != 3 {
		t.Fatalf("unselected folders discarded: %v %+v", err, initial)
	}
	mailboxes, err := db.ListLogicalMailboxes(ctx, "owner", "a")
	if err != nil || len(mailboxes) != 3 {
		t.Fatalf("folders automatically merged before selection: %v %+v", err, mailboxes)
	}
	policy := folderpolicy.Policy{Groups: []folderpolicy.Group{{ID: "inbox", Name: "Inbox", Primary: "[Gmail]/Inbox", Folders: []string{"INBOX", "[Gmail]/Inbox"}}}, Excluded: []string{"Archive"}}
	if err := db.SaveFolderPolicy(ctx, "owner", "a", 1, policy); err != nil {
		t.Fatal(err)
	}
	mailboxes, err = db.ListLogicalMailboxes(ctx, "owner", "a")
	if err != nil || len(mailboxes) != 1 || mailboxes[0].Key != "/groups/inbox" || mailboxes[0].Name != "Inbox" || len(mailboxes[0].Members) != 2 || mailboxes[0].PrimaryRemote != "[Gmail]/Inbox" {
		t.Fatalf("explicit group not listed once with primary/members: %v %+v", err, mailboxes)
	}
	remote, local, err := db.GroupDestination(ctx, "owner", "a", "/groups/inbox")
	if err != nil || remote != "[Gmail]/Inbox" || local != "GmailInbox" {
		t.Fatalf("primary destination guessed or logical key leaked: %q %q %v", remote, local, err)
	}
	if _, _, err := db.GroupDestination(ctx, "owner", "a", "/groups/deleted"); err == nil {
		t.Fatal("stale group became a remote write destination")
	}
	var all []MailSummary
	var cursor *MailCursor
	for i := 0; i < 3; i++ {
		page, err := db.ListMailPage(ctx, "owner", "a", "/groups/inbox", false, cursor, 1)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, page.Messages...)
		if page.Next == nil {
			break
		}
		cursor = page.Next
	}
	if len(all) != 2 || all[0].ID == all[1].ID {
		t.Fatalf("group collapsed distinct messages or repeated page: %+v", all)
	}
	for _, message := range all {
		if message.UID != 7 || message.Path != "/mail/"+message.Folder || (message.Folder == "Inbox" && message.UIDValidity != 100) || (message.Folder == "GmailInbox" && message.UIDValidity != 101) {
			t.Fatalf("logical group overwrote physical identity: %+v", message)
		}
	}
	for _, unread := range []bool{false, true} {
		messages, err := db.ListMail(ctx, "owner", unread)
		if err != nil || len(messages) != 2 {
			t.Fatalf("global list unread=%v: %v %+v", unread, err, messages)
		}
		messages, err = db.ListMailForAccount(ctx, "owner", "a", "", unread)
		if err != nil || len(messages) != 2 {
			t.Fatalf("account list unread=%v: %v %+v", unread, err, messages)
		}
		page, err := db.ListMailPage(ctx, "owner", "", "", unread, nil, 10)
		if err != nil || len(page.Messages) != 2 {
			t.Fatalf("global page unread=%v: %v %+v", unread, err, page)
		}
		page, err = db.ListMailPage(ctx, "owner", "a", "", unread, nil, 10)
		if err != nil || len(page.Messages) != 2 {
			t.Fatalf("account page unread=%v: %v %+v", unread, err, page)
		}
	}
	if err := db.MarkMailRead(ctx, "owner", all[0].ID, true); err != nil {
		t.Fatal(err)
	}
	remaining, err := db.ListMailForAccount(ctx, "owner", "a", "Inbox", true)
	if err != nil || len(remaining) != 1 || remaining[0].ID != all[1].ID {
		t.Fatalf("read applied to wrong group member: %v %+v", err, remaining)
	}
	hidden, err := db.ListMailForAccount(ctx, "owner", "a", "Archive", false)
	if err != nil || len(hidden) != 0 {
		t.Fatalf("explicitly excluded folder visible: %v %+v", err, hidden)
	}
	crossUser, err := db.ListMailForAccount(ctx, "other", "a", "/groups/inbox", false)
	if err != nil || len(crossUser) != 0 {
		t.Fatalf("cross-user grouped query: %v %+v", err, crossUser)
	}
	var count int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM mail_messages`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("group/exclusion deleted stored messages: %d %v", count, err)
	}
}
