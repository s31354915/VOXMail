package store

import (
	"context"
	"errors"
	"github.com/voxmail/voxmail/internal/operation"
	"path/filepath"
	"testing"
)

func TestInterruptedSyncRecoveryPreservesCompletedHistory(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sync.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateUser(ctx, User{ID: "u", Username: "u", PasswordHash: "p", PINHash: "p", Role: "user", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO accounts(id,user_id,canonical_name,email,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,sender_name,created_at) VALUES('a','u','A','a@example.test','imap.test','u','sealed','smtp.test','u','sealed','A','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	completed, err := db.BeginSyncRun(ctx, "a", "initial")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishSyncRun(ctx, completed, true, true, nil); err != nil {
		t.Fatal(err)
	}
	success, err := db.LatestSyncRun(ctx, "a", true)
	if err != nil {
		t.Fatal(err)
	}
	failed, err := db.BeginSyncRun(ctx, "a", "manual")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishSyncRun(ctx, failed, false, false, operation.Wrap("imap", "authenticate", operation.Authentication, errors.New("opaque failure"))); err != nil {
		t.Fatal(err)
	}
	priorFailure, err := db.LatestSyncRun(ctx, "a", false)
	if err != nil {
		t.Fatal(err)
	}
	if priorFailure.ErrorCategory != "authentication" {
		t.Fatalf("lost category: %+v", priorFailure)
	}
	unfinished, err := db.BeginSyncRun(ctx, "a", "manual")
	if err != nil {
		t.Fatal(err)
	}
	// Utility/backup opens must not mislabel a live application's active work.
	other, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	live, err := other.LatestSyncRun(ctx, "a", false)
	if err != nil || live.ID != unfinished || live.FinishedAt != "" {
		t.Fatalf("opening a store altered active work: %+v %v", live, err)
	}
	if n, err := db.InterruptSyncRuns(ctx); err != nil || n != 1 {
		t.Fatalf("recovery: count=%d error=%v", n, err)
	}
	interrupted, err := db.LatestSyncRun(ctx, "a", false)
	if err != nil || interrupted.ID != unfinished || interrupted.FinishedAt == "" || interrupted.Success || interrupted.ErrorCategory != "interrupted" || interrupted.Error != "interrupted by application restart" {
		t.Fatalf("wrong interrupted result: %+v %v", interrupted, err)
	}
	retained, err := db.LatestSyncRun(ctx, "a", true)
	if err != nil || retained != success {
		t.Fatalf("completed success changed: %+v %v", retained, err)
	}
	runs, err := db.ListSyncRuns(ctx, "u", "a", 20)
	if err != nil || len(runs) != 3 || runs[1] != priorFailure {
		t.Fatalf("history changed: %+v %v", runs, err)
	}
	if n, err := db.InterruptSyncRuns(ctx); err != nil || n != 0 {
		t.Fatalf("recovery is not idempotent: %d %v", n, err)
	}
}
