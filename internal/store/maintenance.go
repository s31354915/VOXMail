package store

import (
	"context"
	"fmt"
	"time"
)

// Each statement is its own short transaction and releases the sole database
// connection. A maintenance pass never drains an arbitrary backlog in a loop.
const MaintenanceBatchRows = 256

func (s *Store) Audit(ctx context.Context, userID, action, detail string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO audit_log(user_id, action, detail, created_at) VALUES (?, ?, ?, ?)`, userID, action, detail, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// PruneEphemeral removes security rows that cannot be useful after
// their expiry or successful use. Durable audit, sync, and mail-mutation
// history is intentionally not removed here; those records need an explicit
// operator retention policy rather than an implicit startup deletion.
func (s *Store) PruneEphemeral(ctx context.Context, now time.Time) error {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	nowText := now.UTC().Format(time.RFC3339Nano)
	usedCutoff := now.Add(-24 * time.Hour).UTC().Format(time.RFC3339Nano)
	statements := []struct {
		query string
		args  []any
	}{
		{`DELETE FROM recovery_tokens WHERE id IN (SELECT id FROM recovery_tokens WHERE expires_at <= ? OR (used_at IS NOT NULL AND used_at <= ?) LIMIT ?)`, []any{nowText, usedCutoff, MaintenanceBatchRows}},
		// Older releases retained JSON copies after importing normalized
		// codes. Keep consumed markers until normalization clears that JSON,
		// otherwise cleanup can allow those old copies to be imported again.
		{`DELETE FROM totp_backup_codes WHERE id IN (SELECT id FROM totp_backup_codes WHERE used_at IS NOT NULL AND used_at <= ?
		  AND NOT EXISTS (SELECT 1 FROM users WHERE users.id=totp_backup_codes.user_id
		                  AND COALESCE(users.backup_codes,'')<>'') LIMIT ?)`, []any{usedCutoff, MaintenanceBatchRows}},
		{`DELETE FROM pin_lockouts WHERE phone IN (SELECT phone FROM pin_lockouts WHERE locked_until <= ? LIMIT ?)`, []any{nowText, MaintenanceBatchRows}},
		// An unannounced claim may still belong to an in-flight alert call.
		// Its owner releases it on failure or shutdown; age alone is not proof
		// that the claim can be removed during a running deployment.
	}
	for _, statement := range statements {
		if _, err := s.DB.ExecContext(ctx, statement.query, statement.args...); err != nil {
			return err
		}
	}
	return nil
}

// PruneHistory advances the age and row-count policy in bounded batches.
// Pending, queued, uncertain and unknown states remain available for recovery.
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
	// Explicit terminal sets preserve unknown/future states, active work and
	// unresolved submissions. In particular, unfinished syncs can last longer
	// than the configured history age and must retain their completion row.
	tables := []struct {
		name, key, stamp, terminal string
	}{
		{"audit_log", "id", "created_at", "1=1"},
		{"sync_runs", "id", "finished_at", "finished_at IS NOT NULL AND finished_at<>''"},
		{"message_mutations", "id", "created_at", "status IN ('success','failed','synchronized')"},
		{"outbound_submissions", "message_id", "updated_at", "status IN ('accepted','rejected')"},
		{"voice_install_jobs", "id", "updated_at", "status IN ('complete','failed')"},
		// Failed deletion jobs need operator/retry recovery; keep them.
		{"cleanup_jobs", "id", "updated_at", "status='complete'"},
	}
	for _, table := range tables {
		tableCutoff := cutoff
		if table.name == "voice_install_jobs" {
			// Match the voice API's existing one-hour terminal-job policy.
			tableCutoff = now.Add(-time.Hour).UTC().Format(time.RFC3339Nano)
		}
		prefix := `DELETE FROM ` + table.name + ` WHERE ` + table.key + ` IN (SELECT ` + table.key + ` FROM ` + table.name + ` WHERE ` + table.terminal
		order := ` ORDER BY ` + table.stamp + ` DESC,` + table.key + ` DESC`
		if _, err := s.DB.ExecContext(ctx, prefix+` AND `+table.stamp+` < ? LIMIT ?)`, tableCutoff, MaintenanceBatchRows); err != nil {
			return err
		}
		if _, err := s.DB.ExecContext(ctx, prefix+order+` LIMIT ? OFFSET ?)`, MaintenanceBatchRows, policy.MaxRows); err != nil {
			return err
		}
	}
	return nil
}
