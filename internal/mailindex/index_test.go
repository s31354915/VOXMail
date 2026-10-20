package mailindex

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
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

func TestIndexContinuesAfterMalformedMessageAndPreservesSeenRows(t *testing.T) {
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
	first := filepath.Join(root, "01-first")
	middle := filepath.Join(root, "02-malformed")
	message := []byte("From: a@e\r\nSubject: indexed\r\n\r\nbody\r\n")
	if err := os.WriteFile(first, message, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(middle, message, 0600); err != nil {
		t.Fatal(err)
	}
	var diagnostics bytes.Buffer
	indexer := Indexer{Store: db, Log: slog.New(slog.NewTextHandler(&diagnostics, nil))}
	if err := indexer.Index(context.Background(), "a-marker", filepath.Dir(filepath.Dir(root))); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM mail_messages WHERE account_id='a-marker'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("initial indexed count=%d, want 2", count)
	}
	stale := filepath.Join(filepath.Dir(filepath.Dir(root)), "Inbox", "new", "stale")
	if _, err := db.DB.Exec(`INSERT INTO mail_messages(account_id,folder,path,updated_at) VALUES(?,?,?,?)`, "a-marker", "Inbox", stale, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(middle, []byte("Content-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64\r\n\r\nnot base64!"), 0600); err != nil {
		t.Fatal(err)
	}
	last := filepath.Join(root, "03-last")
	if err := os.WriteFile(last, []byte("From: c@e\r\nSubject: after malformed\r\n\r\nbody\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := indexer.Index(context.Background(), "a-marker", filepath.Dir(filepath.Dir(root))); err != nil {
		t.Fatalf("scan should continue past malformed message: %v", err)
	}
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM mail_messages WHERE path=?`, stale).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("completed scan did not purge an unrelated stale index row")
	}
	var preservedSubject string
	if err := db.DB.QueryRow(`SELECT subject FROM mail_messages WHERE path=?`, middle).Scan(&preservedSubject); err != nil {
		t.Fatalf("previously indexed malformed message was purged: %v", err)
	}
	if preservedSubject != "indexed" {
		t.Fatalf("malformed message overwrote its last valid metadata: %q", preservedSubject)
	}
	var lastSubject string
	if err := db.DB.QueryRow(`SELECT subject FROM mail_messages WHERE path=?`, last).Scan(&lastSubject); err != nil {
		t.Fatalf("valid message after malformed message was not indexed: %v", err)
	}
	if lastSubject != "after malformed" {
		t.Fatalf("last message subject=%q", lastSubject)
	}
	if output := diagnostics.String(); !strings.Contains(output, "mail index message failed strict parse; keeping existing record") || !strings.Contains(output, middle) || !strings.Contains(output, "a-marker") {
		t.Fatalf("parse failure diagnostic lacks account/path context: %q", output)
	}
	if strings.Contains(diagnostics.String(), "mail index skipped malformed message") {
		t.Fatalf("a preserved record must not be logged as skipped: %q", diagnostics.String())
	}
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM mail_messages WHERE account_id='a-marker'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("final indexed count=%d, want first, preserved malformed, and final valid messages", count)
	}
}

// A never-indexed file the lenient parser recovers is indexed and logged as
// recovered, never as skipped.
func TestIndexRecoveredMessageIsNotLoggedAsSkipped(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.DB.Exec(`INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('u-recover','u-recover','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a-recover','u-recover','a','a@e','a','h','u','p','h','u','p','{}','now')`); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "mail", "Inbox", "new")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	damaged := filepath.Join(root, "01-damaged")
	// Corrupt base64 fails the strict decoder but yields its decoded prefix
	// through the lenient reader.
	if err := os.WriteFile(damaged, []byte("From: a@e\r\nSubject: salvaged\r\nContent-Type: text/plain\r\nContent-Transfer-Encoding: base64\r\n\r\nSGVsbG8g###@@@d29ybGQ="), 0600); err != nil {
		t.Fatal(err)
	}
	var diagnostics bytes.Buffer
	indexer := Indexer{Store: db, Log: slog.New(slog.NewTextHandler(&diagnostics, nil))}
	if err := indexer.Index(context.Background(), "a-recover", filepath.Dir(filepath.Dir(root))); err != nil {
		t.Fatal(err)
	}
	var subject string
	if err := db.DB.QueryRow(`SELECT subject FROM mail_messages WHERE path=?`, damaged).Scan(&subject); err != nil {
		t.Fatalf("recovered message was not indexed: %v", err)
	}
	if subject != "salvaged" {
		t.Fatalf("recovered subject=%q", subject)
	}
	output := diagnostics.String()
	if !strings.Contains(output, "mail index recovered damaged message via lenient parse") {
		t.Fatalf("recovery was not logged as recovered: %q", output)
	}
	if strings.Contains(output, "mail index skipped malformed message") {
		t.Fatalf("a recovered message must not be logged as skipped: %q", output)
	}
}

// A never-indexed file no recovery path can read is the only case that
// logs the skipped-malformed warning.
func TestIndexUnrecoverableMessageWarnsSkipped(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.DB.Exec(`INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('u-giveup','u-giveup','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a-giveup','u-giveup','a','a@e','a','h','u','p','h','u','p','{}','now')`); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "mail", "Inbox", "new")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	huge := filepath.Join(root, "01-huge")
	body := bytes.Repeat([]byte("x"), (25<<20)+64)
	if err := os.WriteFile(huge, append([]byte("From: a@e\r\nSubject: big\r\n\r\n"), body...), 0600); err != nil {
		t.Fatal(err)
	}
	var diagnostics bytes.Buffer
	indexer := Indexer{Store: db, Log: slog.New(slog.NewTextHandler(&diagnostics, nil))}
	if err := indexer.Index(context.Background(), "a-giveup", filepath.Dir(filepath.Dir(root))); err != nil {
		t.Fatal(err)
	}
	output := diagnostics.String()
	if !strings.Contains(output, "mail index skipped malformed message") || !strings.Contains(output, huge) || !strings.Contains(output, "a-giveup") {
		t.Fatalf("unrecoverable message was not logged as skipped with context: %q", output)
	}
	if strings.Contains(output, "mail index recovered damaged message via lenient parse") {
		t.Fatalf("oversize message must not be logged as recovered: %q", output)
	}
	var count int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM mail_messages WHERE path=?`, huge).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("oversize message was indexed despite failed recovery")
	}
}

func TestNoteIndexSkipWarnsOncePerPath(t *testing.T) {
	if !noteIndexSkip("acct", "/mail/cur/damaged:2,S") {
		t.Fatal("first report for a path should warn")
	}
	if noteIndexSkip("acct", "/mail/cur/damaged:2,S") {
		t.Fatal("repeat report for the same path should stay silent")
	}
	if !noteIndexSkip("other", "/mail/cur/damaged:2,S") {
		t.Fatal("a different account should warn again")
	}
}
