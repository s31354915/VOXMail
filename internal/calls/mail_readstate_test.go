package calls

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/voxmail/voxmail/internal/store"
)

func writeMailFile(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("From: a@example.com\nSubject: t\n\nbody"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAdvanceToPlayableSkipsUnavailableMessages(t *testing.T) {
	svc, _ := newTestService(t)
	dir := t.TempDir()
	sess := &session{UserID: "u13", Messages: []store.MailSummary{
		{ID: 1, Path: writeMailFile(t, dir, "a")},
		{ID: 2, Path: filepath.Join(dir, "missing")},
		{ID: 3, Path: writeMailFile(t, dir, "c")},
	}, Cursor: 0}
	m := svc.advanceToPlayable(sess)
	if sess.Cursor != 2 {
		t.Fatalf("cursor=%d, want 2", sess.Cursor)
	}
	if m.ID != 3 {
		t.Fatalf("returned message id=%d, want 3", m.ID)
	}
}

func TestOpenSessionMessageResolvesStalePath(t *testing.T) {
	svc, db := newTestService(t)
	dir := t.TempDir()
	if _, err := db.DB.Exec(`INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('u13','u13','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a13','u13','a','a@e','a','h','u','p','h','u','p','{}','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO mail_messages(account_id,folder,path,message_id,is_read,updated_at) VALUES('a13','Inbox','` + filepath.Join(dir, "gone") + `','<m@example.com>',0,'earlier')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO mail_messages(account_id,folder,path,message_id,is_read,updated_at) VALUES('a13','Inbox','` + writeMailFile(t, dir, "real:2,S") + `','<m@example.com>',1,'now')`); err != nil {
		t.Fatal(err)
	}
	var staleID int64
	if err := db.DB.QueryRow(`SELECT id FROM mail_messages WHERE path=?`, filepath.Join(dir, "gone")).Scan(&staleID); err != nil {
		t.Fatal(err)
	}
	sess := &session{UserID: "u13"}
	file, resolved, err := svc.openSessionMessage(sess, store.MailSummary{ID: staleID, Path: filepath.Join(dir, "gone")})
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	want := writeMailFile(t, dir, "real:2,S")
	if resolved != want {
		t.Fatalf("resolved=%q, want %q", resolved, want)
	}
}

func TestSignedInPromptSingularUnread(t *testing.T) {
	svc, db := newTestService(t)
	if _, err := db.DB.Exec(`INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('u14','u14','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a14','u14','Work','w@e','w','h','u','p','h','u','p','{}','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO mail_messages(account_id,folder,path,message_id,is_read,updated_at) VALUES('a14','Inbox','/tmp/none','<m14@example.com>',0,'now')`); err != nil {
		t.Fatal(err)
	}
	got := svc.signedInPrompt(&session{UserID: "u14"})
	if !strings.HasPrefix(got, "You have 1 unread email, ") {
		t.Fatalf("singular grammar broken: %q", got)
	}
}

// readMessage spends the unread marker only after audio has played: the first
// prompt must go out while the message is still unread in the store.
func TestReadMessageMarksReadOnlyAfterAudioPlays(t *testing.T) {
	svc, db := newTestService(t)
	dir := t.TempDir()
	path := writeMailFile(t, dir, "msg1")
	if _, err := db.DB.Exec(`INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('u15','u15','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a15','u15','Work','w@e','w','h','u','p','h','u','p','{}','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO mail_messages(account_id,folder,path,message_id,is_read,updated_at) VALUES('a15','Inbox','` + path + `','<m15@example.com>',0,'now')`); err != nil {
		t.Fatal(err)
	}
	var msgID int64
	if err := db.DB.QueryRow(`SELECT id FROM mail_messages WHERE message_id='<m15@example.com>'`).Scan(&msgID); err != nil {
		t.Fatal(err)
	}
	m := store.MailSummary{ID: msgID, Path: path, Read: false, Folder: "Inbox", AccountID: "a15"}
	sess := &session{UserID: "u15", Messages: []store.MailSummary{m}}
	prompts := 0
	readDuringFirstPrompt := -1
	svc.promptFn = func(_ *session, text string) error {
		prompts++
		if prompts == 1 {
			var isRead int
			if err := db.DB.QueryRow(`SELECT is_read FROM mail_messages WHERE id=?`, msgID).Scan(&isRead); err != nil {
				t.Fatal(err)
			}
			readDuringFirstPrompt = isRead
		}
		return nil
	}
	svc.readMessage(sess, m)
	if prompts == 0 {
		t.Fatal("no prompt played")
	}
	if readDuringFirstPrompt != 0 {
		t.Fatalf("message was marked read before the first prompt played (is_read=%d)", readDuringFirstPrompt)
	}
	var isRead int
	if err := db.DB.QueryRow(`SELECT is_read FROM mail_messages WHERE id=?`, msgID).Scan(&isRead); err != nil {
		t.Fatal(err)
	}
	if isRead != 1 {
		t.Fatalf("message not marked read after playback: is_read=%d", isRead)
	}
}

func TestAdvanceToPlayableCountsSkips(t *testing.T) {
	svc, _ := newTestService(t)
	dir := t.TempDir()
	sess := &session{UserID: "u16", Messages: []store.MailSummary{
		{ID: 1, Path: writeMailFile(t, dir, "a")},
		{ID: 2, Path: filepath.Join(dir, "missing1")},
		{ID: 3, Path: filepath.Join(dir, "missing2")},
		{ID: 4, Path: writeMailFile(t, dir, "d")},
	}, Cursor: 0}
	var prompts []string
	svc.promptFn = func(_ *session, text string) error {
		prompts = append(prompts, text)
		return nil
	}
	m := svc.advanceToPlayable(sess)
	if m.ID != 4 {
		t.Fatalf("returned message id=%d, want 4", m.ID)
	}
	if len(prompts) != 1 || prompts[0] != "Skipping 2 messages that are no longer available." {
		t.Fatalf("skip notice: %q", prompts)
	}
}
