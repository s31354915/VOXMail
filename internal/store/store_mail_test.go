package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestListMailUnreadOnlyUsesMappedInbox(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('u','u','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a','u','Work','a@example.com','Work','imap.example','u','p','smtp.example','u','p','{"INBOX":"Inbox","Junk":"Spam","Trash":"Trash"}','now')`); err != nil {
		t.Fatal(err)
	}
	for i, folder := range []string{"Inbox", "Spam", "Trash"} {
		if _, err := db.DB.ExecContext(ctx, `INSERT INTO mail_messages(account_id,folder,path,subject,is_read,updated_at) VALUES('a',?,?,0,0,'now')`, folder, filepath.Join(t.TempDir(), string(rune('a'+i)))); err != nil {
			t.Fatal(err)
		}
	}
	messages, err := db.ListMail(ctx, "u", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Folder != "Inbox" {
		t.Fatalf("unread result=%+v, want only Inbox", messages)
	}
}

func TestUpdateMailIdentitySurvivesRemoteFolderChange(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('u2','u2','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a2','u2','a','a@e','a','h','u','p','h','u','p','{}','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO mail_messages(account_id,folder,path,message_id,is_read,updated_at) VALUES('a2','Inbox','/mail/old','<stable@example.com>',0,'now')`); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateMailIdentity(ctx, "a2", "Archive/Projects", "<stable@example.com>", 42, 7, []string{"\\Seen"}); err != nil {
		t.Fatal(err)
	}
	var folder, path, flags string
	var read, uid, validity int
	if err := db.DB.QueryRowContext(ctx, `SELECT folder,path,message_flags,is_read,imap_uid,uid_validity FROM mail_messages WHERE account_id='a2'`).Scan(&folder, &path, &flags, &read, &uid, &validity); err != nil {
		t.Fatal(err)
	}
	if folder != "Archive/Projects" || path != "/mail/old" || flags != "\\Seen" || read != 1 || uid != 42 || validity != 7 {
		t.Fatalf("identity update=%q %q %q read=%d uid=%d validity=%d", folder, path, flags, read, uid, validity)
	}
}

func TestListMailFoldersIncludesEmptyDiscoveredFolders(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('u3','u3','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a3','u3','a','a@e','a','h','u','p','h','u','p','{}','now')`); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertRemoteFolder(ctx, "a3", "Archive/Projects", "Archive/Projects", ""); err != nil {
		t.Fatal(err)
	}
	folders, err := db.ListMailFolders(ctx, "u3", "a3")
	if err != nil {
		t.Fatal(err)
	}
	if len(folders) != 1 || folders[0] != "Archive/Projects" {
		t.Fatalf("folders=%v, want discovered empty folder", folders)
	}
}

func TestUpdateMailIdentityByUIDUpdatesFlagsWithoutMessageID(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('u4','u4','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a4','u4','a','a@e','a','h','u','p','h','u','p','{}','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO mail_messages(account_id,folder,path,imap_uid,uid_validity,is_read,updated_at) VALUES('a4','Inbox','/mail/no-message-id',9,4,0,'now')`); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateMailIdentityByUID(ctx, "a4", "Inbox", 9, 4, []string{"\\Seen", "\\Answered"}); err != nil {
		t.Fatal(err)
	}
	var flags string
	var read int
	if err := db.DB.QueryRowContext(ctx, `SELECT message_flags,is_read FROM mail_messages WHERE account_id='a4'`).Scan(&flags, &read); err != nil {
		t.Fatal(err)
	}
	if flags != "\\Seen \\Answered" || read != 1 {
		t.Fatalf("flags=%q read=%d", flags, read)
	}
}

func TestPruneRemoteFoldersRemovesDeletedFolders(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('u5','u5','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a5','u5','a','a@e','a','h','u','p','h','u','p','{}','now')`); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertRemoteFolder(ctx, "a5", "INBOX", "Inbox", "inbox"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertRemoteFolder(ctx, "a5", "Archive", "Archive", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.PruneRemoteFolders(ctx, "a5", []string{"INBOX"}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM remote_folders WHERE account_id='a5'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("remaining folder count=%d, want 1", count)
	}
}
