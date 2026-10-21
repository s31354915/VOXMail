package calls

import (
	"strings"
	"testing"

	"github.com/voxmail/voxmail/internal/ivr"
	"github.com/voxmail/voxmail/internal/store"
)

// The unread entry point must say how many unread messages are waiting before
// the list starts, instead of jumping straight into "Message 1 of N".
func TestOpenUnreadAnnouncesCountSummary(t *testing.T) {
	svc, db := newTestService(t)
	if _, err := db.DB.Exec(`INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('ux1','ux1','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('aux1','ux1','Work','w@e','w','h','u','p','h','u','p','{}','now')`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := db.DB.Exec(`INSERT INTO mail_messages(account_id,folder,path,message_id,is_read,updated_at) VALUES('aux1','Inbox','/tmp/ux-` + string(rune('a'+i)) + `','<ux` + string(rune('a'+i)) + `@example.com>',0,'now')`); err != nil {
			t.Fatal(err)
		}
	}
	sess := &session{UserID: "ux1"}
	var got []string
	svc.promptFn = func(_ *session, text string) error {
		got = append(got, text)
		return nil
	}
	svc.openMessages(sess, true, "all unread mail")
	if len(got) != 1 {
		t.Fatalf("prompts=%d, want 1: %q", len(got), got)
	}
	if !strings.HasPrefix(got[0], "You have 2 unread messages.") {
		t.Fatalf("unread summary missing: %q", got[0])
	}
	if !strings.Contains(got[0], "Message 1 of 2") {
		t.Fatalf("list prompt missing after summary: %q", got[0])
	}
}

func TestOpenUnreadZeroAnnouncesNoUnread(t *testing.T) {
	svc, db := newTestService(t)
	if _, err := db.DB.Exec(`INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('ux2','ux2','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	sess := &session{UserID: "ux2"}
	var got []string
	svc.promptFn = func(_ *session, text string) error {
		got = append(got, text)
		return nil
	}
	svc.openMessages(sess, true, "all unread mail")
	if len(got) != 1 {
		t.Fatalf("prompts=%d, want 1: %q", len(got), got)
	}
	if got[0] != "You have no unread messages. Press pound to go back." {
		t.Fatalf("zero-unread prompt: %q", got[0])
	}
}

// Pressing 5 on the account menu announces the account's unread count and
// repeats the menu, without entering the list.
func TestAccountMenuUnreadCountShortcut(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rows  int
		start string
	}{
		{name: "singular", rows: 1, start: "Work has 1 unread message."},
		{name: "plural", rows: 3, start: "Work has 3 unread messages."},
		{name: "zero", rows: 0, start: "Work has no unread messages."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, db := newTestService(t)
			if _, err := db.DB.Exec(`INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('ux3','ux3','p','p','user','now')`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.DB.Exec(`INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('aux3','ux3','Work','w@e','w','h','u','p','h','u','p','{}','now')`); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < tc.rows; i++ {
				if _, err := db.DB.Exec(`INSERT INTO mail_messages(account_id,folder,path,message_id,is_read,updated_at) VALUES('aux3','Inbox','/tmp/ux3-` + string(rune('a'+i)) + `','<ux3` + string(rune('a'+i)) + `@example.com>',0,'now')`); err != nil {
					t.Fatal(err)
				}
			}
			account := store.Account{ID: "aux3", CanonicalName: "Work"}
			sess := &session{CallID: "ux3-call", UserID: "ux3", ActiveAccount: "aux3", State: "account_menu", Accounts: []store.Account{account}}
			var got []string
			svc.promptFn = func(_ *session, text string) error {
				got = append(got, text)
				return nil
			}
			svc.handleSelectedAccountMenu(sess, "5")
			if len(got) != 1 {
				t.Fatalf("prompts=%d, want 1: %q", len(got), got)
			}
			if !strings.HasPrefix(got[0], tc.start) {
				t.Fatalf("count prompt: %q, want prefix %q", got[0], tc.start)
			}
			if !strings.Contains(got[0], "Press 1 to listen to mail") {
				t.Fatalf("menu not repeated after count: %q", got[0])
			}
			if sess.State != "account_menu" {
				t.Fatalf("state=%q, want account_menu", sess.State)
			}
		})
	}
}

// The IVR contract accepts 5 on the account menu and the spoken menu names it.
func TestAccountMenuSpecAcceptsCountShortcut(t *testing.T) {
	spec, ok := ivr.Spec("account_menu")
	if !ok {
		t.Fatal("account_menu spec missing")
	}
	if !spec.Valid("5") {
		t.Fatal("spec rejects key 5 on account_menu")
	}
	if !strings.Contains(spec.Prompt, "5 for the unread count") {
		t.Fatalf("spec prompt does not mention the shortcut: %q", spec.Prompt)
	}
}
