package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestPruneEphemeralRemovesOnlyExpiredSecurityRows(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "retention.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.CreateBootstrapUser(ctx, User{ID: "u1", Username: "alice", PasswordHash: "p", PINHash: "p", Role: "admin", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	old := now.Add(-48 * time.Hour).Format(time.RFC3339Nano)
	future := now.Add(48 * time.Hour).Format(time.RFC3339Nano)
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO recovery_tokens(user_id,email,token_hash,purpose,expires_at,created_at) VALUES(?,?,?,?,?,?)`, "u1", "a@example.com", "old", "password", old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO recovery_tokens(user_id,email,token_hash,purpose,expires_at,created_at) VALUES(?,?,?,?,?,?)`, "u1", "a@example.com", "future", "password", future, old); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO totp_backup_codes(user_id,code_hash,used_at,created_at) VALUES(?,?,?,?)`, "u1", "old-code", old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO totp_backup_codes(user_id,code_hash,used_at,created_at) VALUES(?,?,?,?)`, "u1", "live-code", nil, old); err != nil {
		t.Fatal(err)
	}
	if err := db.SetPinLockout(ctx, "+15551234567", now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	if err := db.PruneEphemeral(ctx, now); err != nil {
		t.Fatal(err)
	}
	var recovery, backup, lockouts int
	if err := db.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM recovery_tokens`).Scan(&recovery); err != nil {
		t.Fatal(err)
	}
	if err := db.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM totp_backup_codes`).Scan(&backup); err != nil {
		t.Fatal(err)
	}
	if err := db.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM pin_lockouts`).Scan(&lockouts); err != nil {
		t.Fatal(err)
	}
	if recovery != 1 || backup != 1 || lockouts != 0 {
		t.Fatalf("remaining rows recovery=%d backup=%d lockouts=%d", recovery, backup, lockouts)
	}
}

func TestPruneHistoryKeepsInFlightRowsAndBoundsTerminalHistory(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.CreateBootstrapUser(ctx, User{ID: "history-user", Username: "history", PasswordHash: "p", PINHash: "p", Role: "admin", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,created_at) VALUES('history-account','history-user','history','history@example.com','History','imap','u','p','smtp','u','p','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO mail_messages(account_id,folder,path,subject,updated_at) VALUES('history-account','Inbox','/history/message','history','now')`); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	old := now.Add(-90 * 24 * time.Hour).Format(time.RFC3339Nano)
	recent := now.Add(-10 * 24 * time.Hour).Format(time.RFC3339Nano)
	for index, created := range []string{old, recent, recent, recent} {
		if _, err := db.DB.ExecContext(ctx, `INSERT INTO audit_log(user_id,action,detail,created_at) VALUES(?,?,?,?)`, "history-user", "test", fmt.Sprintf("audit-%d", index), created); err != nil {
			t.Fatal(err)
		}
		if _, err := db.DB.ExecContext(ctx, `INSERT INTO sync_runs(account_id,kind,started_at,finished_at,success,changed,error) VALUES(?,?,?,?,?,?,?)`, "history-account", "manual", created, created, 1, 0, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO message_mutations(message_id,user_id,operation,status,created_at) VALUES(?,?,?,?,?)`, 1, "history-user", "delete", "failed", old); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 3; index++ {
		if _, err := db.DB.ExecContext(ctx, `INSERT INTO message_mutations(message_id,user_id,operation,status,created_at) VALUES(?,?,?,?,?)`, 1, "history-user", fmt.Sprintf("delete-%d", index), "success", recent); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO message_mutations(message_id,user_id,operation,status,created_at) VALUES(?,?,?,?,?)`, 1, "history-user", "mark_read", "queued", old); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO message_mutations(message_id,user_id,operation,status,created_at) VALUES(?,?,?,?,?)`, 1, "history-user", "mark_read", "uncertain", recent); err != nil {
		t.Fatal(err)
	}
	for index, status := range []string{"accepted", "accepted", "accepted"} {
		if _, err := db.DB.ExecContext(ctx, `INSERT INTO outbound_submissions(message_id,user_id,account_id,status,created_at,updated_at) VALUES(?,?,?,?,?,?)`, fmt.Sprintf("<recent-%d@example.com>", index), "history-user", "history-account", status, recent, recent); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO outbound_submissions(message_id,user_id,account_id,status,created_at,updated_at) VALUES(?,?,?,?,?,?)`, "<old@example.com>", "history-user", "history-account", "accepted", old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO outbound_submissions(message_id,user_id,account_id,status,created_at,updated_at) VALUES(?,?,?,?,?,?)`, "<pending@example.com>", "history-user", "history-account", "pending", old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO outbound_submissions(message_id,user_id,account_id,status,created_at,updated_at) VALUES(?,?,?,?,?,?)`, "<uncertain@example.com>", "history-user", "history-account", "uncertain", recent, recent); err != nil {
		t.Fatal(err)
	}

	if err := db.PruneHistory(ctx, now, HistoryRetentionPolicy{MaxAge: 30 * 24 * time.Hour, MaxRows: 2}); err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		table string
		want  int
	}{
		{"audit_log", 2},
		{"sync_runs", 2},
		{"message_mutations", 4},
		{"outbound_submissions", 4},
	}
	for _, check := range checks {
		var count int
		if err := db.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+check.table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != check.want {
			t.Errorf("%s rows=%d, want %d", check.table, count, check.want)
		}
	}
}

func TestRetentionBoundsBacklogAndResumesAfterCanceledPass(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "bounded-retention.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.CreateUser(ctx, User{ID: "owner", Username: "owner", PasswordHash: "p", PINHash: "p", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	old := now.Add(-48 * time.Hour).Format(time.RFC3339Nano)
	const total = 5 * MaintenanceBatchRows
	tx, err := db.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := 0; i < total; i++ {
		if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log(user_id,action,detail,created_at) VALUES('owner','test','',?)`, old); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO recovery_tokens(user_id,email,token_hash,purpose,expires_at,created_at) VALUES('owner','owner@example.com',?,'password',?,?)`, fmt.Sprintf("token-%d", i), old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	policy := HistoryRetentionPolicy{MaxAge: 24 * time.Hour, MaxRows: 1}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := db.PruneHistory(canceled, now, policy); err == nil {
		t.Fatal("canceled history pass succeeded")
	}
	if err := db.PruneEphemeral(canceled, now); err == nil {
		t.Fatal("canceled security pass succeeded")
	}
	var audit, security int
	readCounts := func() {
		t.Helper()
		if err := db.DB.QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&audit); err != nil {
			t.Fatal(err)
		}
		if err := db.DB.QueryRow(`SELECT COUNT(*) FROM recovery_tokens`).Scan(&security); err != nil {
			t.Fatal(err)
		}
	}
	readCounts()
	if audit != total || security != total {
		t.Fatal("canceled pass deleted rows")
	}
	if err := db.PruneHistory(ctx, now, policy); err != nil {
		t.Fatal(err)
	}
	if err := db.PruneEphemeral(ctx, now); err != nil {
		t.Fatal(err)
	}
	readCounts()
	if audit != total-2*MaintenanceBatchRows || security != total-MaintenanceBatchRows {
		t.Fatalf("unbounded pass: audit=%d security=%d", audit, security)
	}
	for range 6 {
		if err := db.PruneHistory(ctx, now, policy); err != nil {
			t.Fatal(err)
		}
		if err := db.PruneEphemeral(ctx, now); err != nil {
			t.Fatal(err)
		}
		// An unrelated read remains usable between bounded passes on the same
		// single-connection store; no maintenance transaction spans passes.
		readCtx, cancel := context.WithTimeout(ctx, time.Second)
		if _, err := db.UserByID(readCtx, "owner"); err != nil {
			t.Fatal(err)
		}
		cancel()
	}
	readCounts()
	if audit != 0 || security != 0 {
		t.Fatalf("backlog failed to converge: audit=%d security=%d", audit, security)
	}
}

func TestRetentionFailureIsReportedAndCanRetry(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "retention-fault.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	old := now.Add(-48 * time.Hour).Format(time.RFC3339Nano)
	if _, err := db.DB.Exec(`INSERT INTO audit_log(action,detail,created_at) VALUES('test','',?)`, old); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`CREATE TRIGGER fail_audit_prune BEFORE DELETE ON audit_log BEGIN SELECT RAISE(ABORT,'injected retention failure'); END`); err != nil {
		t.Fatal(err)
	}
	policy := HistoryRetentionPolicy{MaxAge: 24 * time.Hour, MaxRows: 1}
	if err := db.PruneHistory(ctx, now, policy); err == nil {
		t.Fatal("injected maintenance failure was hidden")
	}
	if _, err := db.DB.Exec(`DROP TRIGGER fail_audit_prune`); err != nil {
		t.Fatal(err)
	}
	if err := db.PruneHistory(ctx, now, policy); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("retry failed: count=%d err=%v", count, err)
	}
}
