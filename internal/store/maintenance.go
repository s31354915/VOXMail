package store

import (
	"context"
	"fmt"
	"time"
)

func (s *Store) Audit(ctx context.Context, userID, action, detail string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO audit_log(user_id, action, detail, created_at) VALUES (?, ?, ?, ?)`, userID, action, detail, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// PruneEphemeral removes security and claim rows that cannot be useful after
// their expiry or successful use. Durable audit, sync, and mail-mutation
// history is intentionally not removed here; those records need an explicit
// operator retention policy rather than an implicit startup deletion.
func (s *Store) PruneEphemeral(ctx context.Context, now time.Time) error {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	nowText := now.UTC().Format(time.RFC3339Nano)
	usedCutoff := now.Add(-24 * time.Hour).UTC().Format(time.RFC3339Nano)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	statements := []struct {
		query string
		args  []any
	}{
		{`DELETE FROM recovery_tokens WHERE expires_at <= ? OR (used_at IS NOT NULL AND used_at <= ?)`, []any{nowText, usedCutoff}},
		{`DELETE FROM totp_backup_codes WHERE used_at IS NOT NULL AND used_at <= ?`, []any{usedCutoff}},
		{`DELETE FROM pin_lockouts WHERE locked_until <= ?`, []any{nowText}},
		{`DELETE FROM alert_claims WHERE announced_at IS NULL AND claimed_at <= ?`, []any{usedCutoff}},
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement.query, statement.args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PruneHistory removes only terminal durable history older than MaxAge and
// then keeps the newest MaxRows terminal rows per table. Pending, queued, and
// uncertain records remain available for reconciliation and idempotency.
func (s *Store) PruneHistory(ctx context.Context, now time.Time, policy HistoryRetentionPolicy) error {
	if policy.MaxAge <= 0 {
		return fmt.Errorf("history retention age must be positive")
	}
	if policy.MaxRows <= 0 {
		return fmt.Errorf("history retention row limit must be positive")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	cutoff := now.Add(-policy.MaxAge).UTC().Format(time.RFC3339Nano)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	terminal := "status NOT IN ('queued','pending','uncertain')"
	statements := []struct {
		query string
		args  []any
	}{
		{`DELETE FROM audit_log WHERE created_at < ?`, []any{cutoff}},
		{`DELETE FROM sync_runs WHERE started_at < ?`, []any{cutoff}},
		{`DELETE FROM message_mutations WHERE created_at < ? AND ` + terminal, []any{cutoff}},
		{`DELETE FROM outbound_submissions WHERE created_at < ? AND ` + terminal, []any{cutoff}},
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement.query, statement.args...); err != nil {
			return err
		}
	}
	quotaStatements := []string{
		`DELETE FROM audit_log WHERE id IN (SELECT id FROM audit_log ORDER BY created_at DESC,id DESC LIMIT -1 OFFSET ?)`,
		`DELETE FROM sync_runs WHERE id IN (SELECT id FROM sync_runs ORDER BY started_at DESC,id DESC LIMIT -1 OFFSET ?)`,
		`DELETE FROM message_mutations WHERE ` + terminal + ` AND id IN (SELECT id FROM message_mutations WHERE ` + terminal + ` ORDER BY created_at DESC,id DESC LIMIT -1 OFFSET ?)`,
		`DELETE FROM outbound_submissions WHERE ` + terminal + ` AND message_id IN (SELECT message_id FROM outbound_submissions WHERE ` + terminal + ` ORDER BY created_at DESC,message_id DESC LIMIT -1 OFFSET ?)`,
	}
	for _, query := range quotaStatements {
		if _, err := tx.ExecContext(ctx, query, policy.MaxRows); err != nil {
			return err
		}
	}
	return tx.Commit()
}
