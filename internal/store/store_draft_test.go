package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestDraftRoundTripAndOwnership(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "drafts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('draft-owner','owner','p','p','user','now'),('draft-other','other','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,created_at) VALUES('draft-account','draft-owner','Work','owner@example.com','Owner','imap','owner','p','smtp','owner','p','now')`); err != nil {
		t.Fatal(err)
	}
	draft := DraftRecord{ID: "draft-1", UserID: "draft-owner", AccountID: "draft-account", Subject: "Subject", Body: "Body", ForwardMode: "forward", To: []string{"to@example.com"}, Cc: []string{"cc@example.com"}, Bcc: []string{"bcc@example.com"}, Attachments: []DraftAttachment{{Filename: "voice.wav", ContentType: "audio/wav", Path: "/data/drafts/draft-1-0.bin", Size: 4}}}
	if err := db.SaveDraft(ctx, draft); err != nil {
		t.Fatal(err)
	}
	loaded, err := db.LoadDraft(ctx, "draft-owner", "draft-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Subject != draft.Subject || loaded.Body != draft.Body || loaded.ForwardMode != draft.ForwardMode || len(loaded.To) != 1 || len(loaded.Cc) != 1 || len(loaded.Bcc) != 1 || len(loaded.Attachments) != 1 {
		t.Fatalf("loaded draft=%+v", loaded)
	}
	if _, err := db.LoadDraft(ctx, "draft-other", "draft-1"); err == nil {
		t.Fatal("another user could load the draft")
	}
	drafts, err := db.ListDrafts(ctx, "draft-owner", "draft-account")
	if err != nil || len(drafts) != 1 {
		t.Fatalf("draft list=%+v err=%v", drafts, err)
	}
	if err := db.DeleteDraft(ctx, "draft-other", "draft-1"); err == nil {
		t.Fatal("another user could delete the draft")
	}
	if err := db.DeleteDraft(ctx, "draft-owner", "draft-1"); err != nil {
		t.Fatal(err)
	}
}
