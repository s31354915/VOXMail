package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveDraftGeneratesUniqueRandomIDs(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "drafts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('draft-owner','owner','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,created_at) VALUES('draft-account','draft-owner','Work','owner@example.com','Owner','imap','owner','p','smtp','owner','p','now')`); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for i := 0; i < 50; i++ {
		// SaveDraft takes the record by value and assigns the generated ID
		// internally; read it back from the store.
		if err := db.SaveDraft(ctx, DraftRecord{UserID: "draft-owner", AccountID: "draft-account", Subject: "s"}); err != nil {
			t.Fatal(err)
		}
		drafts, err := db.ListDrafts(ctx, "draft-owner", "draft-account")
		if err != nil {
			t.Fatal(err)
		}
		if len(drafts) != i+1 {
			t.Fatalf("draft count=%d, want %d", len(drafts), i+1)
		}
		latest := drafts[0].ID
		if !strings.HasPrefix(latest, "draft-") {
			t.Fatalf("generated ID %q lacks the draft prefix", latest)
		}
		if seen[latest] {
			t.Fatalf("generated ID %q repeated", latest)
		}
		seen[latest] = true
	}
}

func TestInboxFolderKeepsExcludedRoleHiddenPath(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "inbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('u','u','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a','u','Work','w@example.com','Work','imap','u','p','smtp','u','p','{"INBOX":"Inbox"}','now')`); err != nil {
		t.Fatal(err)
	}
	account := Account{ID: "a", UserID: "u", FolderMap: `{"INBOX":"Inbox"}`}
	got, err := db.InboxFolder(ctx, account)
	if err != nil || got != "Inbox" {
		t.Fatalf("InboxFolder=%q err=%v, want Inbox", got, err)
	}
}
