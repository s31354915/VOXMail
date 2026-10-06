package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestOutboundSubmissionJournalsStableMessageIDAndOutcome(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "submissions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('submit-user','submit','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,created_at) VALUES('submit-account','submit-user','Work','sender@example.com','Sender','imap','sender','p','smtp','sender','p','now')`); err != nil {
		t.Fatal(err)
	}
	want := OutboundSubmission{MessageID: "<attempt-1@example.com>", UserID: "submit-user", AccountID: "submit-account", DraftID: "draft-1", Status: "pending"}
	if err := db.CreateOutboundSubmission(ctx, want); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateOutboundSubmission(ctx, want); err == nil {
		t.Fatal("duplicate Message-ID journal was accepted")
	}
	got, err := db.OutboundSubmission(ctx, want.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if got.MessageID != want.MessageID || got.Status != "pending" || got.DraftID != want.DraftID {
		t.Fatalf("pending submission=%+v", got)
	}
	if err := db.UpdateOutboundSubmission(ctx, want.MessageID, "accepted", ""); err != nil {
		t.Fatal(err)
	}
	got, err = db.OutboundSubmission(ctx, want.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "accepted" || got.AcceptedAt == "" {
		t.Fatalf("accepted submission=%+v", got)
	}
	if err := db.UpdateOutboundSubmission(ctx, "<missing@example.com>", "accepted", ""); err != sql.ErrNoRows {
		t.Fatalf("missing update error=%v, want sql.ErrNoRows", err)
	}
}
