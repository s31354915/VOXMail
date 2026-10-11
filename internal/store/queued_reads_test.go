package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestReadMutationSnapshotSurvivesIndexRefreshAndDatabaseReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mail.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := db.DB.Exec(`INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('u','u','p','p','user','now'); INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,created_at) VALUES('a','u','Mail','u@e','User','h','u','p','h','u','p','now'); INSERT INTO mail_messages(id,account_id,folder,path,is_read,imap_uid,uid_validity,updated_at) VALUES(1,'a','Inbox','/message',1,7,101,'now')`); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordMessageMutation(ctx, 1, "u", "mark_read", "Inbox", "Inbox", "queued", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`UPDATE mail_messages SET is_read=0,imap_uid=8,uid_validity=102 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	watermark, err := db.QueuedMutationWatermark(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	items, err := db.QueuedReadMutations(ctx, "a", watermark)
	if err != nil || len(items) != 1 || !items[0].Read || items[0].UID != 7 || items[0].UIDValidity != 101 {
		t.Fatalf("durable requested identity/state lost: %v err=%v", items, err)
	}
	if err := db.MarkQueuedMutationsSynchronized(ctx, "a", watermark, []string{"Inbox"}); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := db.DB.QueryRow(`SELECT status FROM message_mutations`).Scan(&status); err != nil || status != "queued" {
		t.Fatalf("changed identity falsely acknowledged: %s %v", status, err)
	}
}

func TestLegacyQueuedReadMigrationRetainsJournalAndUsesIndexedFallback(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.DB.Exec(`DELETE FROM schema_migrations WHERE version=26; ALTER TABLE message_mutations DROP COLUMN desired_read; ALTER TABLE message_mutations DROP COLUMN source_uid; ALTER TABLE message_mutations DROP COLUMN source_uid_validity;
 INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('u','u','p','p','user','now'); INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,created_at) VALUES('a','u','Mail','u@e','User','h','u','p','h','u','p','now'); INSERT INTO mail_messages(id,account_id,folder,path,is_read,imap_uid,uid_validity,updated_at) VALUES(1,'a','Inbox','/message',1,7,101,'now'); INSERT INTO message_mutations(message_id,user_id,operation,from_folder,to_folder,status,created_at) VALUES(1,'u','mark_read','Inbox','Inbox','queued','now')`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	items, err := db.QueuedReadMutations(ctx, "a", 100)
	if err != nil || len(items) != 1 || !items[0].Read || items[0].UID != 7 || items[0].UIDValidity != 101 {
		t.Fatalf("legacy pending row lost: %v err=%v", items, err)
	}
}

func TestQueuedReadRecordsRequestedFlagAndRefusesStaleOrForeignSource(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.DB.Exec(`INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('u','u','p','p','user','now'); INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,created_at) VALUES('a','u','Mail','u@e','User','h','u','p','h','u','p','now'); INSERT INTO mail_messages(id,account_id,folder,path,is_read,imap_uid,uid_validity,updated_at) VALUES(1,'a','Inbox','/message',1,7,101,'now')`); err != nil {
		t.Fatal(err)
	}
	item := MailSummary{ID: 1, AccountID: "a", Folder: "Inbox", Path: "/message", Read: true, UID: 7, UIDValidity: 101}
	if err := db.RecordQueuedRead(ctx, "u", item, false); err != nil {
		t.Fatal(err)
	}
	pending, err := db.QueuedReadMutations(ctx, "a", 100)
	if err != nil || len(pending) != 1 || pending[0].Read || !pending[0].CacheRead {
		t.Fatalf("index overwrote requested unread: %v %v", pending, err)
	}
	if err := db.RecordQueuedRead(ctx, "other", item, false); err == nil {
		t.Fatal("foreign owner accepted")
	}
	stale := item
	stale.UIDValidity = 100
	if err := db.RecordQueuedRead(ctx, "u", stale, false); err == nil {
		t.Fatal("stale UIDVALIDITY accepted")
	}
	stale = item
	stale.Path = "/stale"
	if err := db.RecordQueuedRead(ctx, "u", stale, false); err == nil {
		t.Fatal("stale physical path accepted")
	}
}
