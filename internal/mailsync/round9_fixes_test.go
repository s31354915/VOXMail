package mailsync

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	remoteimap "github.com/voxmail/voxmail/internal/imap"
	"github.com/voxmail/voxmail/internal/store"
)

// One unreadable message (a folder whose mbsync state cannot be read) must not
// abort the reconcile: the healthy row still gets its identity refreshed.
func TestRound9ReconcileContinuesPastBrokenFolderState(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('u-r9','r9','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a-r9','u-r9','Work','work@example.com','Work','imap.example','user','sealed','smtp.example','user','sealed','{}','now')`); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "mail", "a-r9")
	inbox := filepath.Join(root, "Inbox")
	broken := filepath.Join(root, "Broken")
	for _, dir := range []string{filepath.Join(inbox, "cur"), filepath.Join(inbox, "new"), filepath.Join(inbox, "tmp")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	// A directory where the state file belongs makes ReadFile fail, which is
	// exactly the per-row failure the reconcile must survive.
	if err := os.MkdirAll(filepath.Join(broken, ".mbsyncstate"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inbox, ".mbsyncstate"), []byte("FarUidValidity 77\nMaxPulledUid 124\nNearUidValidity 88\nMaxExpiredFarUid 0\nMaxPushedUid 0\n\n456 123 S\n457 124\n"), 0600); err != nil {
		t.Fatal(err)
	}
	goodPath := filepath.Join(inbox, "cur", "0.1_keep,U=123:2,S")
	if err := os.WriteFile(goodPath, []byte("mail"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO mail_messages(account_id,folder,path,updated_at) VALUES('a-r9','Broken',?,'now')`, filepath.Join(broken, "cur", "0.1_broken,U=9:2,S")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO mail_messages(account_id,folder,path,updated_at) VALUES('a-r9','Inbox',?,'now')`, goodPath); err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: db, Root: filepath.Dir(filepath.Dir(root))}
	remote := map[string]struct{}{remoteIdentityKey("Inbox", remoteimap.MessageIdentity{UID: 456, UIDValidity: 77}): {}}
	if err := service.reconcileIndexedMessages(ctx, store.Account{ID: "a-r9", UserID: "u-r9"}, remote); err != nil {
		t.Fatalf("reconcile failed on one unreadable message: %v", err)
	}
	var uid, validity uint32
	if err := db.DB.QueryRowContext(ctx, `SELECT imap_uid,uid_validity FROM mail_messages WHERE path=?`, goodPath).Scan(&uid, &validity); err != nil {
		t.Fatal(err)
	}
	if uid != 456 || validity != 77 {
		t.Fatalf("indexed identity=%d/%d, want far UID 456/77 refreshed", uid, validity)
	}
}
