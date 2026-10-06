package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestAccountDeletionHidesAndPersistsOwnedCleanup(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.CreateUser(ctx, User{ID: "delete-user", Username: "delete-user", PasswordHash: "p", PINHash: "p", Role: "user", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('delete-account','delete-user','Work','work@example.com','Work','imap','user','sealed','smtp','user','sealed','{}','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO drafts(id,user_id,account_id,created_at,updated_at) VALUES('delete-draft','delete-user','delete-account','now','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO draft_attachments(draft_id,filename,content_type,path,size) VALUES('delete-draft','voice.wav','audio/wav',?,4)`, filepath.Join(t.TempDir(), "draft.bin")); err != nil {
		t.Fatal(err)
	}
	maildir := filepath.Join(t.TempDir(), "mail", "delete-account")
	job, err := db.BeginAccountDeletion(ctx, "delete-user", "delete-account", maildir)
	if err != nil {
		t.Fatal(err)
	}
	if job.ID == "" || len(job.Paths) != 2 {
		t.Fatalf("cleanup job = %#v, want maildir and attachment paths", job)
	}
	if accounts, err := db.ListAccounts(ctx, "delete-user"); err != nil {
		t.Fatal(err)
	} else if len(accounts) != 0 {
		t.Fatalf("deleting account remained visible: %#v", accounts)
	}
	if _, err := db.BeginAccountDeletion(ctx, "delete-user", "delete-account", maildir); !errors.Is(err, ErrAccountDeleting) {
		t.Fatalf("repeat deletion error = %v, want ErrAccountDeleting", err)
	}
	if err := db.FinalizeAccountDeletion(ctx, "delete-user", "delete-account"); err != nil {
		t.Fatal(err)
	}
	cleanupErr := errors.New("simulated filesystem failure")
	if err := db.FinishCleanupJob(ctx, job.ID, cleanupErr); err != nil {
		t.Fatal(err)
	}
	var status string
	var attempts int
	if err := db.DB.QueryRowContext(ctx, `SELECT status,attempts FROM cleanup_jobs WHERE id=?`, job.ID).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || attempts != 1 {
		t.Fatalf("cleanup status=%q attempts=%d, want failed/1", status, attempts)
	}
	var accountCount int
	if err := db.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE id='delete-account'`).Scan(&accountCount); err != nil {
		t.Fatal(err)
	}
	if accountCount != 0 {
		t.Fatalf("account metadata survived finalization: %d", accountCount)
	}
}

func TestAccountDeletionDoesNotRevealForeignOrMissingIDs(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	for _, user := range []User{
		{ID: "delete-owner", Username: "delete-owner", PasswordHash: "p", PINHash: "p", Role: "user", Enabled: true},
		{ID: "delete-other", Username: "delete-other", PasswordHash: "p", PINHash: "p", Role: "user", Enabled: true},
	} {
		if err := db.CreateUser(ctx, user); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('foreign-account','delete-other','Work','work@example.com','Work','imap','user','sealed','smtp','user','sealed','{}','now')`); err != nil {
		t.Fatal(err)
	}
	for _, accountID := range []string{"foreign-account", "missing-account"} {
		if _, err := db.BeginAccountDeletion(ctx, "delete-owner", accountID, ""); !errors.Is(err, ErrAccountNotFound) {
			t.Fatalf("account %q error=%v, want ErrAccountNotFound", accountID, err)
		}
	}
}
