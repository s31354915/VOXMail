package store

import (
	"context"
	"fmt"
	"net/mail"
	"strings"
	"time"
)

func (s *Store) Migrate(ctx context.Context) error {
	_, err := s.DB.ExecContext(ctx, `
PRAGMA foreign_keys = ON;
PRAGMA journal_mode = WAL;
CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS users (
 id TEXT PRIMARY KEY, username TEXT NOT NULL UNIQUE, password_hash TEXT NOT NULL,
 pin_hash TEXT NOT NULL, role TEXT NOT NULL DEFAULT 'user', enabled INTEGER NOT NULL DEFAULT 1,
 totp_secret TEXT, backup_codes TEXT, created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS caller_whitelist (
 id INTEGER PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 phone TEXT NOT NULL UNIQUE, created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS accounts (
 id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 canonical_name TEXT NOT NULL, email TEXT NOT NULL, sender_name TEXT NOT NULL,
 imap_host TEXT NOT NULL, imap_port INTEGER NOT NULL DEFAULT 993, imap_user TEXT NOT NULL,
 imap_password TEXT NOT NULL, imap_security TEXT NOT NULL DEFAULT 'implicit_tls', smtp_host TEXT NOT NULL, smtp_port INTEGER NOT NULL DEFAULT 465,
 smtp_security TEXT NOT NULL DEFAULT 'implicit_tls',
 smtp_user TEXT NOT NULL, smtp_password TEXT NOT NULL, folder_map TEXT NOT NULL DEFAULT '{}',
	 sync_interval_minutes INTEGER NOT NULL DEFAULT 5, reconciliation_interval_minutes INTEGER NOT NULL DEFAULT 1440, initial_cutoff TEXT, retention_days INTEGER,
 call_alert_enabled INTEGER NOT NULL DEFAULT 0, alert_folders TEXT NOT NULL DEFAULT '[]',
 display_order INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS contacts (
 id INTEGER PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 name TEXT NOT NULL, email TEXT NOT NULL, display_order INTEGER NOT NULL DEFAULT 0,
 UNIQUE(user_id, email)
);
CREATE TABLE IF NOT EXISTS settings (
	user_id TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
	tts_voice TEXT NOT NULL DEFAULT 'en_US-hfc_male-medium',
	menu_speed INTEGER NOT NULL DEFAULT 3, email_speed INTEGER NOT NULL DEFAULT 2,
	alerts_enabled INTEGER NOT NULL DEFAULT 0, alert_phone TEXT,
	alert_next_dial TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS system_settings (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	alerts_available INTEGER NOT NULL DEFAULT 1,
	setup_completed INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS sip_settings (
 id INTEGER PRIMARY KEY CHECK (id = 1),
 domain TEXT NOT NULL DEFAULT '', username TEXT NOT NULL DEFAULT '',
 password TEXT NOT NULL DEFAULT '', port INTEGER NOT NULL DEFAULT 5060,
 transport TEXT NOT NULL DEFAULT 'udp', reg_interval INTEGER NOT NULL DEFAULT 300,
 enabled INTEGER NOT NULL DEFAULT 0, updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS audit_log (
 id INTEGER PRIMARY KEY, user_id TEXT, action TEXT NOT NULL, detail TEXT,
 created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS mail_messages (
 id INTEGER PRIMARY KEY, account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 folder TEXT NOT NULL, path TEXT NOT NULL UNIQUE, message_id TEXT, sender TEXT,
	 recipients TEXT, cc TEXT NOT NULL DEFAULT '', subject TEXT, message_date TEXT, is_read INTEGER NOT NULL DEFAULT 0,
	attachment_count INTEGER NOT NULL DEFAULT 0, alerted INTEGER NOT NULL DEFAULT 0,
	imap_uid INTEGER, uid_validity INTEGER, message_flags TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL, last_seen_scan TEXT NOT NULL DEFAULT '', message_date_utc INTEGER,
	source_size INTEGER NOT NULL DEFAULT -1
);
CREATE TABLE IF NOT EXISTS remote_folders (
 account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 remote_path TEXT NOT NULL, local_path TEXT NOT NULL, parent_path TEXT NOT NULL DEFAULT '',
 depth INTEGER NOT NULL DEFAULT 0, role TEXT NOT NULL DEFAULT '', display_name TEXT NOT NULL,
 updated_at TEXT NOT NULL, PRIMARY KEY(account_id, remote_path)
);
CREATE TABLE IF NOT EXISTS folder_roles (
 account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 role TEXT NOT NULL, remote_path TEXT NOT NULL, local_path TEXT NOT NULL,
 PRIMARY KEY(account_id, role), UNIQUE(account_id, remote_path)
);
CREATE TABLE IF NOT EXISTS drafts (
 id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 subject TEXT NOT NULL DEFAULT '', body TEXT NOT NULL DEFAULT '', forward_mode TEXT NOT NULL DEFAULT '',
 original_message_id INTEGER, created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS draft_recipients (
 id INTEGER PRIMARY KEY, draft_id TEXT NOT NULL REFERENCES drafts(id) ON DELETE CASCADE,
 kind TEXT NOT NULL, address TEXT NOT NULL, display_name TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS draft_attachments (
 id INTEGER PRIMARY KEY, draft_id TEXT NOT NULL REFERENCES drafts(id) ON DELETE CASCADE,
 filename TEXT NOT NULL, content_type TEXT NOT NULL, path TEXT NOT NULL, size INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS recovery_tokens (
 id INTEGER PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 email TEXT NOT NULL, token_hash TEXT NOT NULL UNIQUE, purpose TEXT NOT NULL,
 expires_at TEXT NOT NULL, used_at TEXT, attempts INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS totp_backup_codes (
 id INTEGER PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 code_hash TEXT NOT NULL, used_at TEXT, created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS totp_replays (
 user_id TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
 last_step INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS voice_models (
 id TEXT PRIMARY KEY, voice TEXT NOT NULL UNIQUE, model_path TEXT NOT NULL,
 checksum TEXT NOT NULL, active INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS sync_runs (
 id INTEGER PRIMARY KEY, account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 kind TEXT NOT NULL, started_at TEXT NOT NULL, finished_at TEXT, success INTEGER NOT NULL DEFAULT 0,
 changed INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS alert_numbers (
 id INTEGER PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 number TEXT NOT NULL, active INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL,
 UNIQUE(user_id, number)
);
CREATE TABLE IF NOT EXISTS alert_claims (
 message_id INTEGER PRIMARY KEY REFERENCES mail_messages(id) ON DELETE CASCADE,
 user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 claimed_at TEXT NOT NULL, announced_at TEXT
);
CREATE TABLE IF NOT EXISTS message_mutations (
 id INTEGER PRIMARY KEY, message_id INTEGER NOT NULL REFERENCES mail_messages(id) ON DELETE CASCADE,
 user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE, operation TEXT NOT NULL,
 from_folder TEXT NOT NULL DEFAULT '', to_folder TEXT NOT NULL DEFAULT '', status TEXT NOT NULL,
 error TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS attachment_metadata (
 id INTEGER PRIMARY KEY, message_id INTEGER NOT NULL REFERENCES mail_messages(id) ON DELETE CASCADE,
 part_path TEXT NOT NULL, filename TEXT NOT NULL, content_type TEXT NOT NULL, size INTEGER NOT NULL DEFAULT 0,
 content_id TEXT NOT NULL DEFAULT '', disposition TEXT NOT NULL DEFAULT '', playable INTEGER NOT NULL DEFAULT 0,
 UNIQUE(message_id, part_path)
);
CREATE INDEX IF NOT EXISTS mail_alert_idx ON mail_messages(account_id, folder, is_read, alerted);`)
	if err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	if err := s.applyColumnMigration(ctx, 3, "mail_messages", []struct{ name, declaration string }{
		{"alerted", "INTEGER NOT NULL DEFAULT 0"},
		{"imap_uid", "INTEGER"},
		{"uid_validity", "INTEGER"},
		{"message_flags", "TEXT NOT NULL DEFAULT ''"},
		{"cc", "TEXT NOT NULL DEFAULT ''"},
	}); err != nil {
		return err
	}
	if err := s.applyColumnMigration(ctx, 4, "accounts", []struct{ name, declaration string }{
		{"imap_security", "TEXT NOT NULL DEFAULT 'implicit_tls'"},
		{"smtp_security", "TEXT NOT NULL DEFAULT 'implicit_tls'"},
		{"reconciliation_interval_minutes", "INTEGER NOT NULL DEFAULT 1440"},
	}); err != nil {
		return err
	}
	if err := s.applyColumnMigration(ctx, 5, "system_settings", []struct{ name, declaration string }{
		{"setup_completed", "INTEGER NOT NULL DEFAULT 0"},
	}); err != nil {
		return err
	}
	if _, err := s.DB.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS mail_alert_idx ON mail_messages(account_id, folder, is_read, alerted)`); err != nil {
		return fmt.Errorf("migrate alter table: %w", err)
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE alert_numbers SET active=0 WHERE active=1 AND id NOT IN (SELECT MIN(id) FROM alert_numbers WHERE active=1 GROUP BY user_id)`); err != nil {
		return fmt.Errorf("migrate alert number state: %w", err)
	}
	if _, err := s.DB.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS alert_numbers_one_active ON alert_numbers(user_id) WHERE active=1`); err != nil {
		return fmt.Errorf("migrate alert number index: %w", err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT OR IGNORE INTO system_settings(id, alerts_available, setup_completed) VALUES(1, 1, 0)`); err != nil {
		return fmt.Errorf("migrate system settings: %w", err)
	}
	if err := s.applyMigration(ctx, 6, `UPDATE system_settings SET setup_completed=1 WHERE id=1 AND EXISTS (SELECT 1 FROM users)`); err != nil {
		return err
	}
	if err := s.applyColumnMigration(ctx, 7, "settings", []struct{ name, declaration string }{
		{"alert_next_dial", "TEXT NOT NULL DEFAULT ''"},
	}); err != nil {
		return err
	}
	if err := s.applyColumnMigration(ctx, 8, "mail_messages", []struct{ name, declaration string }{
		{"last_seen_scan", "TEXT NOT NULL DEFAULT ''"},
	}); err != nil {
		return err
	}
	if err := s.applyColumnMigration(ctx, 9, "mail_messages", []struct{ name, declaration string }{
		{"message_date_utc", "INTEGER"},
	}); err != nil {
		return err
	}
	if err := s.applyMigration(ctx, 10, `CREATE INDEX IF NOT EXISTS mail_message_date_idx ON mail_messages(account_id, folder, message_date_utc, id)`); err != nil {
		return err
	}
	if err := s.applyColumnMigration(ctx, 11, "accounts", []struct{ name, declaration string }{
		{"deleting", "INTEGER NOT NULL DEFAULT 0"},
	}); err != nil {
		return err
	}
	if err := s.applyMigration(ctx, 12, `CREATE TABLE IF NOT EXISTS cleanup_jobs (
 id TEXT PRIMARY KEY, user_id TEXT NOT NULL, account_id TEXT NOT NULL,
 paths TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending', attempts INTEGER NOT NULL DEFAULT 0,
 last_error TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL
); CREATE INDEX IF NOT EXISTS cleanup_jobs_status_idx ON cleanup_jobs(status, updated_at)`); err != nil {
		return err
	}
	if err := s.applyMigration(ctx, 13, `CREATE TABLE IF NOT EXISTS outbound_submissions (
 message_id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE, draft_id TEXT NOT NULL DEFAULT '',
 status TEXT NOT NULL, error TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL,
 updated_at TEXT NOT NULL, accepted_at TEXT
); CREATE INDEX IF NOT EXISTS outbound_submissions_user_idx ON outbound_submissions(user_id, created_at)`); err != nil {
		return err
	}
	if err := s.applyColumnMigration(ctx, 14, "accounts", []struct{ name, declaration string }{
		{"config_version", "INTEGER NOT NULL DEFAULT 1"},
	}); err != nil {
		return err
	}
	if err := s.applyColumnMigration(ctx, 15, "sip_settings", []struct{ name, declaration string }{
		{"local_port", "INTEGER NOT NULL DEFAULT 5060"},
		{"registrar_port", "INTEGER NOT NULL DEFAULT 5060"},
	}); err != nil {
		return err
	}
	if err := s.applyMigration(ctx, 16, `UPDATE sip_settings SET local_port=port, registrar_port=port WHERE port <> 5060 AND local_port=5060 AND registrar_port=5060`); err != nil {
		return err
	}
	if err := s.applyColumnMigration(ctx, 17, "mail_messages", []struct{ name, declaration string }{
		{"source_size", "INTEGER NOT NULL DEFAULT -1"},
	}); err != nil {
		return err
	}
	if err := s.backfillMessageDates(ctx); err != nil {
		return err
	}
	if err := s.applyMigration(ctx, 18, `CREATE INDEX IF NOT EXISTS mail_message_page_idx ON mail_messages(account_id, folder, is_read, message_date_utc, id)`); err != nil {
		return err
	}
	if err := s.applyMigration(ctx, 19, `CREATE INDEX IF NOT EXISTS audit_log_retention_idx ON audit_log(created_at, id);
CREATE INDEX IF NOT EXISTS sync_runs_retention_idx ON sync_runs(started_at, id);
CREATE INDEX IF NOT EXISTS message_mutations_retention_idx ON message_mutations(created_at, id);
CREATE INDEX IF NOT EXISTS outbound_submissions_retention_idx ON outbound_submissions(status, created_at, message_id)`); err != nil {
		return err
	}
	if err := s.applyMigration(ctx, 20, `CREATE TABLE IF NOT EXISTS voice_install_jobs (
 id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 voice TEXT NOT NULL, stage TEXT NOT NULL, status TEXT NOT NULL,
 progress INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL, updated_at TEXT NOT NULL
); CREATE INDEX IF NOT EXISTS voice_install_jobs_status_idx ON voice_install_jobs(status, updated_at)`); err != nil {
		return err
	}
	if err := s.applyMigration(ctx, 1, ""); err != nil {
		return err
	}
	if err := s.applyMigration(ctx, 2, `CREATE TABLE IF NOT EXISTS pin_lockouts (
 phone TEXT PRIMARY KEY, locked_until TEXT NOT NULL, updated_at TEXT NOT NULL
)`); err != nil {
		return err
	}
	return nil
}

// NormalizeMessageDate converts the original mail date into a sortable UTC
// Unix timestamp. Invalid or missing headers deliberately return nil; callers
// keep the original header for display and sort such rows after valid dates.
// Future dates are retained as supplied metadata rather than silently
// rewriting a sender's message.
func NormalizeMessageDate(raw string) *int64 {
	parsed, err := mail.ParseDate(strings.TrimSpace(raw))
	if err != nil {
		return nil
	}
	value := parsed.UTC().Unix()
	return &value
}

func (s *Store) backfillMessageDates(ctx context.Context) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin message date backfill: %w", err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id,message_date FROM mail_messages WHERE message_date_utc IS NULL AND COALESCE(message_date,'') <> ''`)
	if err != nil {
		return fmt.Errorf("read message dates: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return err
		}
		value := NormalizeMessageDate(raw)
		if value == nil {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE mail_messages SET message_date_utc=? WHERE id=? AND message_date_utc IS NULL`, *value, id); err != nil {
			return fmt.Errorf("backfill message date %d: %w", id, err)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return tx.Commit()
}

// applyMigration records a numbered schema change exactly once. The initial
// schema remains idempotent for compatibility with existing deployments, but
// all new changes must pass through this table so upgrades are auditable.
func (s *Store) applyMigration(ctx context.Context, version int, statement string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration %d: %w", version, err)
	}
	defer tx.Rollback()
	var present int
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version=?`, version).Scan(&present)
	if err != nil {
		return fmt.Errorf("inspect migration %d: %w", version, err)
	}
	if present != 0 {
		return tx.Commit()
	}
	if strings.TrimSpace(statement) != "" {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply migration %d: %w", version, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,applied_at) VALUES(?,?)`, version, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("record migration %d: %w", version, err)
	}
	return tx.Commit()
}

// applyColumnMigration records compatibility ALTER TABLE work in the same
// migration ledger as ordinary schema changes. Existing deployments that
// received a column through the older ensureColumn path are recognized and
// simply have the migration recorded without attempting a duplicate ALTER.
func (s *Store) applyColumnMigration(ctx context.Context, version int, table string, columns []struct{ name, declaration string }) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin column migration %d: %w", version, err)
	}
	defer tx.Rollback()
	var present int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version=?`, version).Scan(&present); err != nil {
		return fmt.Errorf("inspect column migration %d: %w", version, err)
	}
	if present != 0 {
		return tx.Commit()
	}
	for _, column := range columns {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?`, table, column.name).Scan(&exists); err != nil {
			return fmt.Errorf("inspect column %s.%s: %w", table, column.name, err)
		}
		if exists != 0 {
			continue
		}
		statement := fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, quoteIdent(table), quoteIdent(column.name), column.declaration)
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply column migration %d: %w", version, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,applied_at) VALUES(?,?)`, version, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("record column migration %d: %w", version, err)
	}
	return tx.Commit()
}

// ensureColumn adds a column to an existing table when the schema predates it.
func (s *Store) ensureColumn(ctx context.Context, table, column, declaration string) error {
	var count int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	statement := fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, quoteIdent(table), quoteIdent(column), declaration)
	_, err := s.DB.ExecContext(ctx, statement)
	return err
}

// quoteIdent renders a SQLite identifier safely for use inside a literal ALTER
// TABLE statement; identifiers cannot be bound as parameters.
func quoteIdent(value string) string { return `"` + strings.ReplaceAll(value, `"`, `""`) + `"` }
