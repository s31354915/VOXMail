package calls

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/voxmail/voxmail/internal/bridge"
	"github.com/voxmail/voxmail/internal/store"
)

func TestDuplicateDTMFDoesNotCancelNextPrompt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess := &session{CallID: "call", playCancel: cancel}
	svc := &Service{sessions: map[string]*session{"call": sess}}
	d := newCallEventDispatcher(context.Background(), svc, nil)
	defer d.close()
	d.activeDTMF["call"] = activeTone{"1", time.Now()}
	if err := d.dispatch(bridge.Message{Type: "dtmf", CallID: "call", Digit: "1", Phase: "start"}); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != nil {
		t.Fatal("discarded duplicate canceled the current prompt")
	}
}

func TestLostReleaseAndLegacyPressesDoNotDisableKey(t *testing.T) {
	for _, phase := range []string{"start", ""} {
		t.Run(phase, func(t *testing.T) {
			d := newCallEventDispatcher(context.Background(), &Service{}, nil)
			defer d.close()
			handled := make(chan struct{}, 1)
			d.handleFn = func(context.Context, bridge.Message) error { handled <- struct{}{}; return nil }
			seen := time.Now()
			if phase == "start" {
				seen = seen.Add(-dtmfReleaseGrace - time.Millisecond)
			}
			d.activeDTMF["call"] = activeTone{"1", seen}
			if err := d.dispatch(bridge.Message{Type: "dtmf", CallID: "call", Digit: "1", Phase: phase}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-handled:
			case <-time.After(time.Second):
				t.Fatal("a later press was swallowed")
			}
		})
	}
}

func TestAllUnreadReadMenuAcceptsAdvertisedKeys(t *testing.T) {
	for _, tc := range []struct{ key, state, phrase string }{
		{"1", "read", "Subject subject"}, {"2", "read", "marked unread"},
		{"3", "body_method", "Replying"}, {"4", "body_method", "Reply all"},
		{"5", "forward_options", "Forward the original"}, {"6", "confirm_delete", "Delete this message"},
		{"7", "move_menu", "Move message"}, {"8", "read", "no playable attachments"},
		{"9", "more_options", "More options"}, {"0", "list", "Message 1 of 1"},
		{"#", "list", "Press 1 to read"}, {"*", "read", "mark read or unread"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			svc, db := newTestService(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			svc.ctx = ctx
			svc.InactivityTimeout = time.Hour
			if err := db.CreateUser(ctx, store.User{ID: "u", Username: "u", PasswordHash: "p", PINHash: "p", Role: "user", Enabled: true}); err != nil {
				t.Fatal(err)
			}
			_, err := db.DB.ExecContext(ctx, `INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a','u','Mail','u@example.test','User','imap.example','u','sealed','smtp.example','u','sealed','{"INBOX":"Inbox","Trash":"Trash"}','now')`)
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(svc.DataRoot, "mail", "a", "Inbox", "new")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "message")
			if err := os.WriteFile(path, []byte("From: sender@example.test\nTo: u@example.test\nSubject: subject\n\nThis is the email body."), 0600); err != nil {
				t.Fatal(err)
			}
			_, err = db.DB.ExecContext(ctx, `INSERT INTO mail_messages(account_id,folder,path,sender,subject,is_read,updated_at) VALUES('a','Inbox',?,'sender@example.test','subject',0,'now')`, path)
			if err != nil {
				t.Fatal(err)
			}
			sess := &session{CallID: "unread", UserID: "u", State: "accounts", Authenticated: true}
			svc.sessions[sess.CallID] = sess
			prompts := make(chan string, 10)
			svc.promptFn = func(_ *session, text string) error { prompts <- text; return nil }
			d := newCallEventDispatcher(ctx, svc, nil)
			defer d.close()
			press := func(key, want string) {
				t.Helper()
				if err := d.dispatch(bridge.Message{Type: "dtmf", CallID: sess.CallID, Digit: key, Phase: "start"}); err != nil {
					t.Fatal(err)
				}
				if err := d.dispatch(bridge.Message{Type: "dtmf", CallID: sess.CallID, Phase: "end"}); err != nil {
					t.Fatal(err)
				}
				select {
				case text := <-prompts:
					if !strings.Contains(text, want) {
						t.Fatalf("key %s prompt %q, want %q", key, text, want)
					}
				case <-time.After(2 * time.Second):
					t.Fatalf("key %s was ignored", key)
				}
			}
			press("0", "Message 1 of 1")
			press("1", "Subject subject")
			press(tc.key, tc.phrase)
			svc.mu.Lock()
			state := string(stateLocked(sess))
			svc.mu.Unlock()
			if state != tc.state {
				t.Fatalf("key %s state=%s want %s", tc.key, state, tc.state)
			}
			if tc.key == "2" {
				var read int
				if err := db.DB.QueryRowContext(ctx, `SELECT is_read FROM mail_messages`).Scan(&read); err != nil || read != 0 {
					t.Fatalf("read toggle ignored: read=%d err=%v", read, err)
				}
			}
		})
	}
}
