package calls

import (
	"os"
	"path/filepath"
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
