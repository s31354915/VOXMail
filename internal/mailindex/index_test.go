package mailindex

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/voxmail/voxmail/internal/mailparse"
	"github.com/voxmail/voxmail/internal/store"
)

func BenchmarkMetadataScanLargeMailbox(b *testing.B) {
	const (
		messageCount   = 128
		attachmentSize = 512 << 10
	)
	root := filepath.Join(b.TempDir(), "mail", "INBOX", "new")
	if err := os.MkdirAll(root, 0700); err != nil {
		b.Fatal(err)
	}
	attachment := strings.Repeat("x", attachmentSize)
	for index := 0; index < messageCount; index++ {
		message := fmt.Sprintf("From: sender@example.com\r\nSubject: fixture %d\r\nContent-Type: multipart/mixed; boundary=bench\r\n\r\n--bench\r\nContent-Type: text/plain\r\n\r\nfixture\r\n--bench\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=payload.bin\r\nContent-Transfer-Encoding: 8bit\r\n\r\n%s\r\n--bench--\r\n", index, attachment)
		path := filepath.Join(root, fmt.Sprintf("message-%d", index))
		if err := os.WriteFile(path, []byte(message), 0600); err != nil {
			b.Fatal(err)
		}
	}

	b.ReportAllocs()
	b.SetBytes(int64(messageCount * attachmentSize))
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		count := 0
		err := mailparse.ScanMetadataEachContext(context.Background(), filepath.Dir(filepath.Dir(root)), func(message mailparse.MaildirMessage) error {
			count++
			if len(message.Attachments) != 1 || message.Attachments[0].Size != attachmentSize {
				return fmt.Errorf("unexpected metadata for %s: %+v", message.Path, message.Attachments)
			}
			return nil
		})
		if err != nil {
			b.Fatal(err)
		}
		if count != messageCount {
			b.Fatalf("scanned %d messages, want %d", count, messageCount)
		}
	}
}

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

func TestIndexSkipsUnchangedFileAndReparsesWhenSizeChanges(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.DB.Exec(`INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('u-skip','u-skip','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a-skip','u-skip','a','a@e','a','h','u','p','h','u','p','{}','now')`); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "mail", "Inbox", "new")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "message")
	first := []byte("From: a@e\r\nSubject: first\r\n\r\nbody\r\n")
	if err := os.WriteFile(path, first, 0600); err != nil {
		t.Fatal(err)
	}
	indexer := Indexer{Store: db}
	mailRoot := filepath.Dir(filepath.Dir(root))
	if err := indexer.Index(context.Background(), "a-skip", mailRoot); err != nil {
		t.Fatal(err)
	}
	var sourceSize int64
	if err := db.DB.QueryRow(`SELECT source_size FROM mail_messages WHERE path=?`, path).Scan(&sourceSize); err != nil {
		t.Fatal(err)
	}
	if sourceSize != int64(len(first)) {
		t.Fatalf("source_size=%d, want %d", sourceSize, len(first))
	}
	if _, err := db.DB.Exec(`UPDATE mail_messages SET subject='database sentinel' WHERE path=?`, path); err != nil {
		t.Fatal(err)
	}
	if err := indexer.Index(context.Background(), "a-skip", mailRoot); err != nil {
		t.Fatal(err)
	}
	var subject string
	if err := db.DB.QueryRow(`SELECT subject FROM mail_messages WHERE path=?`, path).Scan(&subject); err != nil {
		t.Fatal(err)
	}
	if subject != "database sentinel" {
		t.Fatalf("unchanged file was reparsed; subject=%q", subject)
	}
	second := []byte("From: a@e\r\nSubject: changed and larger\r\n\r\nbody\r\n")
	if err := os.WriteFile(path, second, 0600); err != nil {
		t.Fatal(err)
	}
	if err := indexer.Index(context.Background(), "a-skip", mailRoot); err != nil {
		t.Fatal(err)
	}
	if err := db.DB.QueryRow(`SELECT subject,source_size FROM mail_messages WHERE path=?`, path).Scan(&subject, &sourceSize); err != nil {
		t.Fatal(err)
	}
	if subject != "changed and larger" || sourceSize != int64(len(second)) {
		t.Fatalf("changed file was not reparsed: subject=%q size=%d", subject, sourceSize)
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

func TestIndexUsesScanMarkerAndFailsClosedOnMalformedMessage(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.DB.Exec(`INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('u-marker','u-marker','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a-marker','u-marker','a','a@e','a','h','u','p','h','u','p','{}','now')`); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "mail", "Inbox", "new")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	message := []byte("From: a@e\r\nSubject: indexed\r\n\r\nbody\r\n")
	if err := os.WriteFile(first, message, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, message, 0600); err != nil {
		t.Fatal(err)
	}
	indexer := Indexer{Store: db}
	if err := indexer.Index(context.Background(), "a-marker", filepath.Dir(filepath.Dir(root))); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(second); err != nil {
		t.Fatal(err)
	}
	if err := indexer.Index(context.Background(), "a-marker", filepath.Dir(filepath.Dir(root))); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM mail_messages WHERE account_id='a-marker'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("successful scan stale count=%d, want 1", count)
	}
	stale := filepath.Join(filepath.Dir(filepath.Dir(root)), "Inbox", "new", "stale")
	if _, err := db.DB.Exec(`INSERT INTO mail_messages(account_id,folder,path,updated_at) VALUES(?,?,?,?)`, "a-marker", "Inbox", stale, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(root, "bad")
	if err := os.WriteFile(bad, []byte("Content-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64\r\n\r\nnot base64!"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := indexer.Index(context.Background(), "a-marker", filepath.Dir(filepath.Dir(root))); err == nil {
		t.Fatal("malformed scan unexpectedly succeeded")
	}
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM mail_messages WHERE path=?`, stale).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("malformed scan purged stale index rows")
	}
}
