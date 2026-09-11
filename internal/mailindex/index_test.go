package mailindex

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

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

func TestPruneLocalNeverTouchesRemoteAndKeepsUnparseableDates(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mail")
	oldDir := filepath.Join(root, "Inbox", "new")
	if err := os.MkdirAll(oldDir, 0700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(oldDir, "old")
	unknown := filepath.Join(oldDir, "unknown")
	oldDate := time.Now().Add(-30 * 24 * time.Hour).UTC().Format(time.RFC1123Z)
	if err := os.WriteFile(old, []byte(fmt.Sprintf("Date: %s\r\nSubject: old\r\n\r\nbody\r\n", oldDate)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unknown, []byte("Date: not-a-date\r\nSubject: keep\r\n\r\nbody\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	days := 7
	removed, err := (Indexer{}).PruneLocal(root, nil, &days)
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("expected old local message to be removed")
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old message still exists, stat error=%v", err)
	}
	if _, err := os.Stat(unknown); err != nil {
		t.Fatalf("unparseable message should be retained: %v", err)
	}
}
