package store

import (
	"context"
	"path/filepath"
	"testing"
)

func seedReadStateAccount(t *testing.T, db *Store, account, user string) {
	t.Helper()
	if _, err := db.DB.Exec(`INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('` + user + `','` + user + `','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('` + account + `','` + user + `','a','a@e','a','h','u','p','h','u','p','{}','now')`); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateMailIdentityKeepsLocalRead(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedReadStateAccount(t, db, "a9", "u9")
	if _, err := db.DB.Exec(`INSERT INTO mail_messages(account_id,folder,path,message_id,is_read,updated_at,imap_uid,uid_validity) VALUES('a9','Inbox','/mail/x:2,S','<x@example.com>',1,'now',42,7)`); err != nil {
		t.Fatal(err)
	}
	// The provider still reports the message unread, but a local read change
	// is queued for the provider: reconciliation must not erase the read.
	if err := db.UpdateMailIdentity(ctx, "a9", "Inbox", "<x@example.com>", 42, 7, nil, true); err != nil {
		t.Fatal(err)
	}
	var read int
	if err := db.DB.QueryRow(`SELECT is_read FROM mail_messages WHERE account_id='a9'`).Scan(&read); err != nil {
		t.Fatal(err)
	}
	if read != 1 {
		t.Fatalf("protected local read was erased: is_read=%d", read)
	}
	if err := db.UpdateMailIdentity(ctx, "a9", "Inbox", "<x@example.com>", 42, 7, nil, false); err != nil {
		t.Fatal(err)
	}
	if err := db.DB.QueryRow(`SELECT is_read FROM mail_messages WHERE account_id='a9'`).Scan(&read); err != nil {
		t.Fatal(err)
	}
	if read != 0 {
		t.Fatalf("server flags were ignored without protection: is_read=%d", read)
	}
}

func TestMailReadCandidatesPrefersCurrentThenSiblings(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedReadStateAccount(t, db, "a10", "u10")
	if _, err := db.DB.Exec(`INSERT INTO mail_messages(account_id,folder,path,message_id,is_read,updated_at) VALUES('a10','Inbox','/gone:2,','<m@example.com>',0,'earlier')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO mail_messages(account_id,folder,path,message_id,is_read,updated_at) VALUES('a10','Inbox','/here:2,S','<m@example.com>',1,'now')`); err != nil {
		t.Fatal(err)
	}
	var staleID int64
	if err := db.DB.QueryRow(`SELECT id FROM mail_messages WHERE path='/gone:2,'`).Scan(&staleID); err != nil {
		t.Fatal(err)
	}
	own, siblings, err := db.MailReadCandidates(ctx, "u10", staleID)
	if err != nil {
		t.Fatal(err)
	}
	if own != "/gone:2," {
		t.Fatalf("own path=%q", own)
	}
	if len(siblings) != 1 || siblings[0] != "/here:2,S" {
		t.Fatalf("siblings=%v", siblings)
	}
	if _, _, err := db.MailReadCandidates(ctx, "someone-else", staleID); err == nil {
		t.Fatal("expected error for another user's message")
	}
}

func TestUpsertMailIndexRehomesDuplicateRow(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedReadStateAccount(t, db, "a11", "u11")
	first, err := db.UpsertMailIndex(ctx, MailIndexInput{AccountID: "a11", Folder: "Inbox", Path: "/old:2,", MessageID: "<m@example.com>", Subject: "Hi", UpdatedAt: "t1", ScanID: "s1", SourceSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	again, err := db.UpsertMailIndex(ctx, MailIndexInput{AccountID: "a11", Folder: "Inbox", Path: "/new:2,S", MessageID: "<m@example.com>", Subject: "Hi", Read: true, UpdatedAt: "t2", ScanID: "s2", SourceSize: 10, PathIsLive: func(candidate string) bool { return candidate != "/old:2," }})
	if err != nil {
		t.Fatal(err)
	}
	if again != first {
		t.Fatalf("re-home created a new row: %d vs %d", again, first)
	}
	var count, read int
	var path string
	if err := db.DB.QueryRow(`SELECT COUNT(*),is_read,path FROM mail_messages WHERE message_id='<m@example.com>'`).Scan(&count, &read, &path); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("duplicate rows remain: %d", count)
	}
	if path != "/new:2,S" || read != 1 {
		t.Fatalf("row not re-homed: path=%q read=%d", path, read)
	}
}

func TestUpsertMailIndexKeepsLiveDuplicateCopies(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedReadStateAccount(t, db, "a12", "u12")
	first, err := db.UpsertMailIndex(ctx, MailIndexInput{AccountID: "a12", Folder: "Inbox", Path: "/copy-a:2,", MessageID: "<m@example.com>", Subject: "Hi", UpdatedAt: "t1", ScanID: "s1", SourceSize: 10, PathIsLive: func(string) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.UpsertMailIndex(ctx, MailIndexInput{AccountID: "a12", Folder: "Inbox", Path: "/copy-b:2,", MessageID: "<m@example.com>", Subject: "Hi", UpdatedAt: "t2", ScanID: "s2", SourceSize: 10, PathIsLive: func(string) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatalf("live duplicate collapsed onto one row: %d", second)
	}
	var count int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM mail_messages WHERE message_id='<m@example.com>'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("expected one row per live copy, got %d", count)
	}
}
