package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestReservedInboxObservationReplacesCaseVariantAndRetainsOldSource(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "inbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.DB.Exec(`INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('owner','owner','p','p','user','now'); INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a','owner','Mail','a@e','A','h','u','p','h','u','p','{"INBOX":"FutureInbox","Sent":"UpperSent","sent":"LowerSent"}','now'); INSERT INTO remote_folders(account_id,remote_path,local_path,parent_path,depth,role,display_name,updated_at) VALUES('a','Inbox','OldInbox','',0,'inbox','Inbox','old'); INSERT INTO mail_messages(account_id,folder,path,message_id,imap_uid,uid_validity,updated_at) VALUES('a','OldInbox','/mail/old','<old@test>',15,99,'old')`); err != nil {
		t.Fatal(err)
	}
	inventory, err := db.FolderInventory(ctx, "owner", "a")
	if err != nil || len(inventory) != 3 || inventory["INBOX"] != "OldInbox" {
		t.Fatalf("case variant obscured observed directory: %v %v", inventory, err)
	}
	if err := db.UpsertRemoteFolder(ctx, "a", "iNbOx", "CurrentInbox", "inbox"); err != nil {
		t.Fatal(err)
	}
	for remote, local := range map[string]string{"Sent": "UpperSent", "sent": "LowerSent"} {
		if err := db.UpsertRemoteFolder(ctx, "a", remote, local, ""); err != nil {
			t.Fatal(err)
		}
	}
	inventory, err = db.FolderInventory(ctx, "owner", "a")
	if err != nil || len(inventory) != 3 || inventory["INBOX"] != "CurrentInbox" || inventory["Sent"] != "UpperSent" || inventory["sent"] != "LowerSent" {
		t.Fatalf("reserved/ordinary identities conflated: %v %v", inventory, err)
	}
	var count int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM remote_folders WHERE account_id='a' AND lower(remote_path)='inbox'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("alternate INBOX observations remain: %d %v", count, err)
	}
	remote, err := db.RemoteFolderForLocal(ctx, "owner", "a", "OldInbox")
	if err != nil || remote != "INBOX" {
		t.Fatalf("old source binding lost: %q %v", remote, err)
	}
	bindings, err := db.FolderBindingsForPreview(ctx, "owner", "a")
	if err != nil || bindings["OldInbox"] != "INBOX" || bindings["CurrentInbox"] != "INBOX" {
		t.Fatalf("preview bindings lost history: %v %v", bindings, err)
	}
	var uid, epoch int
	if err := db.DB.QueryRow(`SELECT imap_uid,uid_validity FROM mail_messages WHERE account_id='a' AND folder='OldInbox'`).Scan(&uid, &epoch); err != nil || uid != 15 || epoch != 99 {
		t.Fatalf("old message identity rewritten: %d %d %v", uid, epoch, err)
	}
}
