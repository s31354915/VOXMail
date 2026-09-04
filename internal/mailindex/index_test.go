package mailindex

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/voxmail/voxmail/internal/store"
)

func TestIndexMaildir(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.DB.Exec(`INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('u','u','p','p','user','now')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.DB.Exec(`INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a','u','a','a@e','a','h','u','p','h','u','p','{}','now')`)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "INBOX", "new")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "1")
	if err := os.WriteFile(path, []byte("From: a@e\r\nSubject: hi\r\n\r\nbody\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := (Indexer{Store: db}).Index(context.Background(), "a", filepath.Dir(filepath.Dir(root))); err != nil {
		t.Fatal(err)
	}
	var count int
	_ = db.DB.QueryRow(`SELECT COUNT(*) FROM mail_messages`).Scan(&count)
	if count != 1 {
		t.Fatalf("count=%d", count)
	}
}
