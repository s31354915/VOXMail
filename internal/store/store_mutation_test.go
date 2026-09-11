package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestRecordMessageMutationRequiresMessageOwnership(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('owner','owner','p','p','user','now'),('other','other','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,created_at) VALUES('a','owner','Work','owner@example.com','Owner','imap','owner','p','smtp','owner','p','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO mail_messages(account_id,folder,path,subject,updated_at) VALUES('a','Inbox','/tmp/message','Subject','now')`); err != nil {
		t.Fatal(err)
	}
	var messageID int64
	if err := db.DB.QueryRowContext(ctx, `SELECT id FROM mail_messages WHERE account_id='a'`).Scan(&messageID); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordMessageMutation(ctx, messageID, "other", "delete", "Inbox", "Trash", "success", nil); err == nil {
		t.Fatal("mutation for another user was accepted")
	}
	if err := db.RecordMessageMutation(ctx, messageID, "owner", "delete", "Inbox", "Trash", "success", nil); err != nil {
		t.Fatalf("owner mutation rejected: %v", err)
	}
	var count int
	if err := db.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM message_mutations WHERE message_id=?`, messageID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("mutation count=%d, want 1", count)
	}
}

func TestQueuedMutationIsReportedAndClosedBySuccessfulSync(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('owner','owner','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,created_at) VALUES('a','owner','Work','owner@example.com','Owner','imap','owner','p','smtp','owner','p','now')`); err != nil {
		t.Fatal(err)
	}
	result, err := db.DB.ExecContext(ctx, `INSERT INTO mail_messages(account_id,folder,path,subject,updated_at) VALUES('a','Inbox','/tmp/message','Subject','now')`)
	if err != nil {
		t.Fatal(err)
	}
	messageID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordMessageMutation(ctx, messageID, "owner", "mark_read", "Inbox", "Inbox", "queued", nil); err != nil {
		t.Fatal(err)
	}
	items, err := db.ListMessageMutations(ctx, "owner", "a", 10)
	if err != nil || len(items) != 1 || items[0].Status != "queued" {
		t.Fatalf("queued mutation report=%+v err=%v", items, err)
	}
	if err := db.MarkQueuedMutationsSynchronized(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	items, err = db.ListMessageMutations(ctx, "owner", "a", 10)
	if err != nil || len(items) != 1 || items[0].Status != "synchronized" {
		t.Fatalf("synchronized mutation report=%+v err=%v", items, err)
	}
}
