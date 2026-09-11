package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/voxmail/voxmail/internal/secret"

	_ "modernc.org/sqlite"
)

type Store struct{ DB *sql.DB }

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{DB: db}
	if err := s.Migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.DB.Close() }

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
 alerts_enabled INTEGER NOT NULL DEFAULT 0, alert_phone TEXT
);
CREATE TABLE IF NOT EXISTS system_settings (
 id INTEGER PRIMARY KEY CHECK (id = 1),
 alerts_available INTEGER NOT NULL DEFAULT 1
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
 updated_at TEXT NOT NULL
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
	if _, err := s.DB.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS mail_alert_idx ON mail_messages(account_id, folder, is_read, alerted)`); err != nil {
		return fmt.Errorf("migrate alter table: %w", err)
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE alert_numbers SET active=0 WHERE active=1 AND id NOT IN (SELECT MIN(id) FROM alert_numbers WHERE active=1 GROUP BY user_id)`); err != nil {
		return fmt.Errorf("migrate alert number state: %w", err)
	}
	if _, err := s.DB.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS alert_numbers_one_active ON alert_numbers(user_id) WHERE active=1`); err != nil {
		return fmt.Errorf("migrate alert number index: %w", err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT OR IGNORE INTO system_settings(id, alerts_available) VALUES(1, 1)`); err != nil {
		return fmt.Errorf("migrate system settings: %w", err)
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

func (s *Store) Healthy(ctx context.Context) error { return s.DB.PingContext(ctx) }

func (s *Store) AlertsAvailable(ctx context.Context) (bool, error) {
	var enabled int
	if err := s.DB.QueryRowContext(ctx, `SELECT alerts_available FROM system_settings WHERE id=1`).Scan(&enabled); err != nil {
		return false, err
	}
	return enabled != 0, nil
}

func (s *Store) SetAlertsAvailable(ctx context.Context, enabled bool) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO system_settings(id, alerts_available) VALUES(1, ?) ON CONFLICT(id) DO UPDATE SET alerts_available=excluded.alerts_available`, enabled)
	return err
}

func (s *Store) Audit(ctx context.Context, userID, action, detail string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO audit_log(user_id, action, detail, created_at) VALUES (?, ?, ?, ?)`, userID, action, detail, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// PinLockout returns the restart-safe lockout deadline for a normalized
// caller identity. Expired rows are removed so this table remains bounded.
func (s *Store) PinLockout(ctx context.Context, phone string) (time.Time, error) {
	phone = strings.TrimSpace(phone)
	if phone == "" {
		return time.Time{}, nil
	}
	var raw string
	err := s.DB.QueryRowContext(ctx, `SELECT locked_until FROM pin_lockouts WHERE phone=?`, phone).Scan(&raw)
	if err == sql.ErrNoRows {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	deadline, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil || !deadline.After(time.Now()) {
		_, _ = s.DB.ExecContext(ctx, `DELETE FROM pin_lockouts WHERE phone=?`, phone)
		return time.Time{}, nil
	}
	return deadline, nil
}

func (s *Store) SetPinLockout(ctx context.Context, phone string, until time.Time) error {
	phone = strings.TrimSpace(phone)
	if phone == "" || until.IsZero() {
		return fmt.Errorf("phone and lockout deadline are required")
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO pin_lockouts(phone,locked_until,updated_at) VALUES(?,?,?) ON CONFLICT(phone) DO UPDATE SET locked_until=excluded.locked_until,updated_at=excluded.updated_at`, phone, until.UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) ClearPinLockout(ctx context.Context, phone string) error {
	phone = strings.TrimSpace(phone)
	if phone == "" {
		return nil
	}
	_, err := s.DB.ExecContext(ctx, `DELETE FROM pin_lockouts WHERE phone=?`, phone)
	return err
}

func (s *Store) IMAPPassword(ctx context.Context, accountID string) (string, error) {
	var value string
	err := s.DB.QueryRowContext(ctx, `SELECT imap_password FROM accounts WHERE id = ?`, accountID).Scan(&value)
	return value, err
}

func (s *Store) SMTPPassword(ctx context.Context, accountID string) (string, error) {
	var value string
	err := s.DB.QueryRowContext(ctx, `SELECT smtp_password FROM accounts WHERE id = ?`, accountID).Scan(&value)
	return value, err
}

type User struct {
	ID           string `json:"id"`
	Username     string `json:"username"`
	PasswordHash string `json:"-"`
	PINHash      string `json:"-"`
	Role         string `json:"role"`
	Enabled      bool   `json:"enabled"`
	TOTPSecret   string `json:"-"`
	BackupCodes  string `json:"-"`
}

type Account struct {
	ID                            string  `json:"id"`
	UserID                        string  `json:"user_id"`
	CanonicalName                 string  `json:"canonical_name"`
	Email                         string  `json:"email"`
	SenderName                    string  `json:"sender_name"`
	IMAPHost                      string  `json:"imap_host"`
	IMAPUser                      string  `json:"imap_user"`
	SMTPHost                      string  `json:"smtp_host"`
	SMTPUser                      string  `json:"smtp_user"`
	IMAPPort                      int     `json:"imap_port"`
	IMAPSecurity                  string  `json:"imap_security"`
	SMTPPort                      int     `json:"smtp_port"`
	SMTPSecurity                  string  `json:"smtp_security"`
	IMAPPassword                  string  `json:"-"`
	SMTPPassword                  string  `json:"-"`
	FolderMap                     string  `json:"folder_map"`
	AlertFolders                  string  `json:"alert_folders"`
	SyncIntervalMinutes           int     `json:"sync_interval_minutes"`
	ReconciliationIntervalMinutes int     `json:"reconciliation_interval_minutes"`
	DisplayOrder                  int     `json:"display_order"`
	InitialCutoff                 *string `json:"initial_cutoff,omitempty"`
	RetentionDays                 *int    `json:"retention_days,omitempty"`
	CallAlertEnabled              bool    `json:"call_alert_enabled"`
}

type FolderRole struct {
	AccountID  string `json:"account_id"`
	Role       string `json:"role"`
	RemotePath string `json:"remote_path"`
	LocalPath  string `json:"local_path"`
}

type DraftAttachment struct {
	Filename    string
	ContentType string
	Path        string
	Size        int64
}

type DraftRecord struct {
	ID                string
	UserID            string
	AccountID         string
	Subject           string
	Body              string
	ForwardMode       string
	OriginalMessageID *int64
	To                []string
	Cc                []string
	Bcc               []string
	Attachments       []DraftAttachment
	CreatedAt         string
	UpdatedAt         string
}

type SyncRun struct {
	ID         int64  `json:"id"`
	AccountID  string `json:"account_id"`
	Kind       string `json:"kind"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at,omitempty"`
	Success    bool   `json:"success"`
	Changed    bool   `json:"changed"`
	Error      string `json:"error,omitempty"`
}

type MessageMutation struct {
	ID         int64  `json:"id"`
	MessageID  int64  `json:"message_id"`
	AccountID  string `json:"account_id"`
	Operation  string `json:"operation"`
	FromFolder string `json:"from_folder"`
	ToFolder   string `json:"to_folder"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
	CreatedAt  string `json:"created_at"`
}

type Contact struct {
	ID           int64  `json:"id"`
	UserID       string `json:"user_id"`
	Name         string `json:"name"`
	Email        string `json:"email"`
	DisplayOrder int    `json:"display_order"`
}

type WhitelistEntry struct {
	ID     int64  `json:"id"`
	UserID string `json:"user_id"`
	Phone  string `json:"phone"`
}

type AlertNumber struct {
	ID        int64  `json:"id"`
	UserID    string `json:"user_id"`
	Number    string `json:"number"`
	Active    bool   `json:"active"`
	CreatedAt string `json:"created_at,omitempty"`
}

type AttachmentMetadata struct {
	PartPath    string
	Filename    string
	ContentType string
	Size        int64
	ContentID   string
	Disposition string
	Playable    bool
}

// SIPSettings is the single deployment-wide baresip account. Password holds
// the sealed ciphertext as read from the database; it is never serialized.
type SIPSettings struct {
	Domain      string `json:"domain"`
	Username    string `json:"username"`
	Password    string `json:"-"`
	Port        int    `json:"port"`
	Transport   string `json:"transport"`
	RegInterval int    `json:"reg_interval"`
	Enabled     bool   `json:"enabled"`
}

type MailSummary struct {
	ID                                                                        int64
	AccountID, Folder, Path, MessageID, Sender, Recipients, Cc, Subject, Date string
	Read                                                                      bool
	Attachments                                                               int
	UID                                                                       uint32
	UIDValidity                                                               uint32
	Flags                                                                     string
}

func (s *Store) ListMail(ctx context.Context, userID string, unreadOnly bool) ([]MailSummary, error) {
	query := `SELECT m.id,m.account_id,m.folder,m.path,COALESCE(m.message_id,''),COALESCE(m.sender,''),COALESCE(m.recipients,''),COALESCE(m.cc,''),COALESCE(m.subject,''),COALESCE(m.message_date,''),m.is_read,m.attachment_count,COALESCE(m.imap_uid,0),COALESCE(m.uid_validity,0),COALESCE(m.message_flags,'') FROM mail_messages m JOIN accounts a ON a.id=m.account_id WHERE a.user_id=?`
	args := []any{userID}
	if unreadOnly {
		accounts, err := s.ListAccounts(ctx, userID)
		if err != nil {
			return nil, err
		}
		if len(accounts) == 0 {
			return []MailSummary{}, nil
		}
		parts := make([]string, 0, len(accounts))
		for _, account := range accounts {
			parts = append(parts, `(m.account_id=? AND m.folder=? AND m.is_read=0)`)
			inbox, err := s.InboxFolder(ctx, account)
			if err != nil {
				return nil, err
			}
			args = append(args, account.ID, inbox)
		}
		query += ` AND (` + strings.Join(parts, ` OR `) + `)`
	}
	query += ` ORDER BY COALESCE(m.message_date,''),m.id DESC`
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MailSummary
	for rows.Next() {
		var m MailSummary
		var read int
		var uid, uidValidity int64
		if err := rows.Scan(&m.ID, &m.AccountID, &m.Folder, &m.Path, &m.MessageID, &m.Sender, &m.Recipients, &m.Cc, &m.Subject, &m.Date, &read, &m.Attachments, &uid, &uidValidity, &m.Flags); err != nil {
			return nil, err
		}
		m.Read = read != 0
		m.UID, m.UIDValidity = uint32(uid), uint32(uidValidity)
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) ListMailForAccount(ctx context.Context, userID, accountID, folder string, unreadOnly bool) ([]MailSummary, error) {
	query := `SELECT m.id,m.account_id,m.folder,m.path,COALESCE(m.message_id,''),COALESCE(m.sender,''),COALESCE(m.recipients,''),COALESCE(m.cc,''),COALESCE(m.subject,''),COALESCE(m.message_date,''),m.is_read,m.attachment_count,COALESCE(m.imap_uid,0),COALESCE(m.uid_validity,0),COALESCE(m.message_flags,'') FROM mail_messages m JOIN accounts a ON a.id=m.account_id WHERE a.user_id=? AND m.account_id=?`
	args := []any{userID, accountID}
	if folder != "" {
		query += ` AND m.folder=?`
		args = append(args, folder)
	}
	if unreadOnly {
		if folder == "" {
			accounts, err := s.ListAccounts(ctx, userID)
			if err != nil {
				return nil, err
			}
			for _, account := range accounts {
				if account.ID == accountID {
					var err error
					folder, err = s.InboxFolder(ctx, account)
					if err != nil {
						return nil, err
					}
					break
				}
			}
			query += ` AND m.folder=?`
			args = append(args, folder)
		}
		query += ` AND m.is_read=0`
	}
	query += ` ORDER BY COALESCE(m.message_date,''),m.id DESC`
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MailSummary
	for rows.Next() {
		var m MailSummary
		var read int
		var uid, uidValidity int64
		if err := rows.Scan(&m.ID, &m.AccountID, &m.Folder, &m.Path, &m.MessageID, &m.Sender, &m.Recipients, &m.Cc, &m.Subject, &m.Date, &read, &m.Attachments, &uid, &uidValidity, &m.Flags); err != nil {
			return nil, err
		}
		m.Read = read != 0
		m.UID, m.UIDValidity = uint32(uid), uint32(uidValidity)
		out = append(out, m)
	}
	return out, rows.Err()
}

// inboxFolder keeps the compatibility folder_map format useful while the
// normalized folder_roles table is introduced. New records should map the
// remote INBOX explicitly; old records continue to use the stable Inbox
// default. This function is intentionally the only fallback used by unread
// queries, so Spam and Trash are never counted by name guessing.
func (s *Store) InboxFolder(ctx context.Context, account Account) (string, error) {
	var local string
	err := s.DB.QueryRowContext(ctx, `SELECT local_path FROM folder_roles WHERE account_id=? AND role='inbox'`, account.ID).Scan(&local)
	if err == nil && strings.TrimSpace(local) != "" {
		return filepathCleanFolder(local), nil
	}
	if err != nil && err != sql.ErrNoRows {
		return "", err
	}
	return inboxFolderLegacy(account), nil
}

// FolderRole returns the explicitly configured remote/local folder pair. It
// intentionally has no name-based guessing: callers must not accidentally
// treat a folder named "Trash" or "Junk" as a role the user did not map.
func (s *Store) FolderRole(ctx context.Context, account Account, role string) (FolderRole, error) {
	var result FolderRole
	err := s.DB.QueryRowContext(ctx, `SELECT account_id,role,remote_path,local_path FROM folder_roles WHERE account_id=? AND role=?`, account.ID, strings.ToLower(strings.TrimSpace(role))).Scan(&result.AccountID, &result.Role, &result.RemotePath, &result.LocalPath)
	if err == nil {
		result.LocalPath = filepathCleanFolder(result.LocalPath)
		return result, nil
	}
	if err != sql.ErrNoRows {
		return FolderRole{}, err
	}
	// Compatibility for pre-role accounts: only an exact conventional remote
	// mapping is accepted, never substring matching such as "Deleted Items".
	var mapping map[string]string
	if json.Unmarshal([]byte(account.FolderMap), &mapping) == nil {
		for remote, local := range mapping {
			if strings.EqualFold(strings.TrimSpace(remote), role) {
				return FolderRole{AccountID: account.ID, Role: role, RemotePath: remote, LocalPath: filepathCleanFolder(local)}, nil
			}
		}
	}
	return FolderRole{}, sql.ErrNoRows
}

func inboxFolderLegacy(account Account) string {
	var mapping map[string]string
	if json.Unmarshal([]byte(account.FolderMap), &mapping) == nil {
		for remote, local := range mapping {
			if strings.EqualFold(strings.TrimSpace(remote), "INBOX") && strings.TrimSpace(local) != "" {
				return filepathCleanFolder(local)
			}
		}
	}
	return "Inbox"
}

func (s *Store) ListFolderRoles(ctx context.Context, userID, accountID string) ([]FolderRole, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT r.account_id,r.role,r.remote_path,r.local_path FROM folder_roles r JOIN accounts a ON a.id=r.account_id WHERE a.user_id=? AND r.account_id=? ORDER BY r.role`, userID, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var roles []FolderRole
	for rows.Next() {
		var role FolderRole
		if err := rows.Scan(&role.AccountID, &role.Role, &role.RemotePath, &role.LocalPath); err != nil {
			return nil, err
		}
		roles = append(roles, role)
	}
	return roles, rows.Err()
}

func (s *Store) SaveFolderRoles(ctx context.Context, userID, accountID string, roles map[string]string, mapping map[string]string) error {
	if accountID == "" {
		return fmt.Errorf("account ID is required")
	}
	var owner string
	if err := s.DB.QueryRowContext(ctx, `SELECT user_id FROM accounts WHERE id=?`, accountID).Scan(&owner); err != nil {
		return err
	} else if owner != userID {
		return fmt.Errorf("account does not belong to user")
	}
	allowed := map[string]bool{"inbox": true, "sent": true, "drafts": true, "spam": true, "trash": true, "archive": true}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM folder_roles WHERE account_id=?`, accountID); err != nil {
		return err
	}
	for role, remote := range roles {
		role = strings.ToLower(strings.TrimSpace(role))
		remote = strings.TrimSpace(remote)
		if !allowed[role] || remote == "" || strings.ContainsAny(remote, "\x00\r\n") {
			return fmt.Errorf("invalid folder role mapping")
		}
		if existing, ok := mapping[remote]; !ok || strings.TrimSpace(existing) == "" {
			// The role table stores the local path independently from the
			// compatibility folder_map. Do not silently create an alias that
			// mbsync was never configured to mirror.
			return fmt.Errorf("folder role %q is missing from folder mappings", role)
		}
		local := strings.TrimSpace(mapping[remote])
		if local == "" {
			local = strings.Title(role)
		}
		local = filepathCleanFolder(local)
		if _, err := tx.ExecContext(ctx, `INSERT INTO folder_roles(account_id,role,remote_path,local_path) VALUES(?,?,?,?)`, accountID, role, remote, local); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) SaveDraft(ctx context.Context, draft DraftRecord) error {
	if draft.UserID == "" || draft.AccountID == "" {
		return fmt.Errorf("draft ownership and account are required")
	}
	if draft.ID == "" {
		draft.ID = fmt.Sprintf("draft-%d", time.Now().UnixNano())
	}
	var owner string
	if err := s.DB.QueryRowContext(ctx, `SELECT user_id FROM accounts WHERE id=?`, draft.AccountID).Scan(&owner); err != nil {
		return err
	} else if owner != draft.UserID {
		return fmt.Errorf("draft account does not belong to user")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `INSERT INTO drafts(id,user_id,account_id,subject,body,forward_mode,original_message_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET subject=excluded.subject,body=excluded.body,forward_mode=excluded.forward_mode,original_message_id=excluded.original_message_id,updated_at=excluded.updated_at`, draft.ID, draft.UserID, draft.AccountID, draft.Subject, draft.Body, draft.ForwardMode, draft.OriginalMessageID, now, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM draft_recipients WHERE draft_id=?`, draft.ID); err != nil {
		return err
	}
	for _, recipient := range draft.To {
		if _, err := tx.ExecContext(ctx, `INSERT INTO draft_recipients(draft_id,kind,address) VALUES(?,?,?)`, draft.ID, "to", recipient); err != nil {
			return err
		}
	}
	for _, recipient := range draft.Cc {
		if _, err := tx.ExecContext(ctx, `INSERT INTO draft_recipients(draft_id,kind,address) VALUES(?,?,?)`, draft.ID, "cc", recipient); err != nil {
			return err
		}
	}
	for _, recipient := range draft.Bcc {
		if _, err := tx.ExecContext(ctx, `INSERT INTO draft_recipients(draft_id,kind,address) VALUES(?,?,?)`, draft.ID, "bcc", recipient); err != nil {
			return err
		}
	}
	for _, attachment := range draft.Attachments {
		if _, err := tx.ExecContext(ctx, `INSERT INTO draft_attachments(draft_id,filename,content_type,path,size) VALUES(?,?,?,?,?)`, draft.ID, attachment.Filename, attachment.ContentType, attachment.Path, attachment.Size); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListDrafts returns durable drafts owned by the user. Account ownership is
// checked in the query as well as by SaveDraft so a deleted or reassigned
// account can never make another user's draft visible.
func (s *Store) ListDrafts(ctx context.Context, userID, accountID string) ([]DraftRecord, error) {
	if strings.TrimSpace(userID) == "" {
		return nil, fmt.Errorf("draft user is required")
	}
	query := `SELECT d.id,d.user_id,d.account_id,d.subject,d.body,d.forward_mode,d.original_message_id,d.created_at,d.updated_at
		FROM drafts d JOIN accounts a ON a.id=d.account_id WHERE d.user_id=? AND a.user_id=?`
	args := []any{userID, userID}
	if strings.TrimSpace(accountID) != "" {
		query += ` AND d.account_id=?`
		args = append(args, accountID)
	}
	query += ` ORDER BY d.updated_at DESC,d.id DESC`
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DraftRecord
	for rows.Next() {
		var draft DraftRecord
		if err := rows.Scan(&draft.ID, &draft.UserID, &draft.AccountID, &draft.Subject, &draft.Body, &draft.ForwardMode, &draft.OriginalMessageID, &draft.CreatedAt, &draft.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, draft)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Load child rows only after the parent result set is closed. SQLite is
	// configured with a single connection, so querying children while rows is
	// still open would wait forever for the same connection.
	rows.Close()
	for i := range out {
		if err := s.loadDraftChildren(ctx, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// LoadDraft loads one draft only when it belongs to userID.
func (s *Store) LoadDraft(ctx context.Context, userID, draftID string) (DraftRecord, error) {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(draftID) == "" {
		return DraftRecord{}, fmt.Errorf("draft identity is required")
	}
	var draft DraftRecord
	err := s.DB.QueryRowContext(ctx, `SELECT d.id,d.user_id,d.account_id,d.subject,d.body,d.forward_mode,d.original_message_id,d.created_at,d.updated_at
		FROM drafts d JOIN accounts a ON a.id=d.account_id WHERE d.id=? AND d.user_id=? AND a.user_id=?`, draftID, userID, userID).
		Scan(&draft.ID, &draft.UserID, &draft.AccountID, &draft.Subject, &draft.Body, &draft.ForwardMode, &draft.OriginalMessageID, &draft.CreatedAt, &draft.UpdatedAt)
	if err != nil {
		return DraftRecord{}, err
	}
	if err := s.loadDraftChildren(ctx, &draft); err != nil {
		return DraftRecord{}, err
	}
	return draft, nil
}

func (s *Store) loadDraftChildren(ctx context.Context, draft *DraftRecord) error {
	recipientRows, err := s.DB.QueryContext(ctx, `SELECT kind,address FROM draft_recipients WHERE draft_id=? ORDER BY id`, draft.ID)
	if err != nil {
		return err
	}
	for recipientRows.Next() {
		var kind, address string
		if err := recipientRows.Scan(&kind, &address); err != nil {
			recipientRows.Close()
			return err
		}
		switch kind {
		case "to":
			draft.To = append(draft.To, address)
		case "cc":
			draft.Cc = append(draft.Cc, address)
		case "bcc":
			draft.Bcc = append(draft.Bcc, address)
		}
	}
	if err := recipientRows.Err(); err != nil {
		recipientRows.Close()
		return err
	}
	recipientRows.Close()
	attachmentRows, err := s.DB.QueryContext(ctx, `SELECT filename,content_type,path,size FROM draft_attachments WHERE draft_id=? ORDER BY id`, draft.ID)
	if err != nil {
		return err
	}
	defer attachmentRows.Close()
	for attachmentRows.Next() {
		var attachment DraftAttachment
		if err := attachmentRows.Scan(&attachment.Filename, &attachment.ContentType, &attachment.Path, &attachment.Size); err != nil {
			return err
		}
		draft.Attachments = append(draft.Attachments, attachment)
	}
	return attachmentRows.Err()
}

func (s *Store) DeleteDraft(ctx context.Context, userID, draftID string) error {
	result, err := s.DB.ExecContext(ctx, `DELETE FROM drafts WHERE id=? AND user_id=? AND account_id IN (SELECT id FROM accounts WHERE user_id=?)`, draftID, userID, userID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("draft was not found")
	}
	return nil
}

func filepathCleanFolder(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	value = strings.Trim(value, "/")
	if value == "" || value == "." || strings.HasPrefix(value, "../") || value == ".." {
		return "Inbox"
	}
	return value
}

func (s *Store) ListMailFolders(ctx context.Context, userID, accountID string) ([]string, error) {
	// Include discovered folders even when they currently contain no indexed
	// messages.  Otherwise an empty but valid destination (or a nested folder
	// whose messages have not arrived yet) disappears from the phone menu.
	rows, err := s.DB.QueryContext(ctx, `SELECT folder FROM (
		SELECT DISTINCT m.folder AS folder
		FROM mail_messages m JOIN accounts a ON a.id=m.account_id
		WHERE a.user_id=? AND m.account_id=?
		UNION
		SELECT DISTINCT f.local_path AS folder
		FROM remote_folders f JOIN accounts a ON a.id=f.account_id
		WHERE a.user_id=? AND f.account_id=? AND f.local_path<>''
	) ORDER BY folder`, userID, accountID, userID, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var folders []string
	for rows.Next() {
		var folder string
		if err := rows.Scan(&folder); err != nil {
			return nil, err
		}
		folders = append(folders, folder)
	}
	return folders, rows.Err()
}

func (s *Store) MarkMailRead(ctx context.Context, userID string, id int64, read bool) error {
	result, err := s.DB.ExecContext(ctx, `UPDATE mail_messages SET is_read=?,updated_at=? WHERE id=? AND account_id IN (SELECT id FROM accounts WHERE user_id=?)`, read, time.Now().UTC().Format(time.RFC3339Nano), id, userID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("message was not found")
	}
	return nil
}

// MarkMailReadAtPath updates the local Maildir path and read state together.
// The path changes when the Maildir filename gains or loses the S flag; both
// values must be durable before mbsync gets a chance to propagate the local
// mutation.
func (s *Store) MarkMailReadAtPath(ctx context.Context, userID string, id int64, read bool, path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("maildir path is required")
	}
	result, err := s.DB.ExecContext(ctx, `UPDATE mail_messages SET is_read=?,path=?,updated_at=? WHERE id=? AND account_id IN (SELECT id FROM accounts WHERE user_id=?)`, read, path, time.Now().UTC().Format(time.RFC3339Nano), id, userID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("message was not found")
	}
	return nil
}

func (s *Store) MoveMailIndex(ctx context.Context, userID string, id int64, folder, path string) error {
	result, err := s.DB.ExecContext(ctx, `UPDATE mail_messages SET folder=?,path=?,updated_at=? WHERE id=? AND account_id IN (SELECT id FROM accounts WHERE user_id=?)`, folder, path, time.Now().UTC().Format(time.RFC3339Nano), id, userID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("message was not found")
	}
	return nil
}

func (s *Store) UpdateMailIdentity(ctx context.Context, accountID, folder, messageID string, uid, uidValidity uint32, flags []string) error {
	if accountID == "" || folder == "" || messageID == "" || uid == 0 || uidValidity == 0 {
		return nil
	}
	// Prefer an already-indexed row in the current local folder, but fall back
	// to the same Message-ID in another folder.  mbsync can rename a Maildir
	// file during a remote move, so requiring both folder and Message-ID would
	// lose the stable IMAP identity precisely when reconciliation is needed.
	var id int64
	err := s.DB.QueryRowContext(ctx, `SELECT id FROM mail_messages WHERE account_id=? AND message_id=? ORDER BY CASE WHEN folder=? THEN 0 ELSE 1 END,id LIMIT 1`, accountID, messageID, folder).Scan(&id)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	seen := 0
	for _, flag := range flags {
		if strings.EqualFold(strings.TrimSpace(flag), imapSeenFlag) {
			seen = 1
			break
		}
	}
	_, err = s.DB.ExecContext(ctx, `UPDATE mail_messages SET folder=?,imap_uid=?,uid_validity=?,message_flags=?,is_read=?,updated_at=? WHERE id=? AND account_id=?`, folder, uid, uidValidity, strings.Join(flags, " "), seen, time.Now().UTC().Format(time.RFC3339Nano), id, accountID)
	return err
}

// UpdateMailIdentityByUID records flags for messages whose provider omitted a
// Message-ID header. UID plus UIDVALIDITY is the only stable identity
// available in that case, and the folder is part of the lookup because UIDs
// are scoped to an IMAP mailbox.
func (s *Store) UpdateMailIdentityByUID(ctx context.Context, accountID, folder string, uid, uidValidity uint32, flags []string) error {
	if accountID == "" || folder == "" || uid == 0 || uidValidity == 0 {
		return nil
	}
	seen := 0
	for _, flag := range flags {
		if strings.EqualFold(strings.TrimSpace(flag), imapSeenFlag) {
			seen = 1
			break
		}
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE mail_messages SET imap_uid=?,uid_validity=?,message_flags=?,is_read=?,updated_at=? WHERE account_id=? AND folder=? AND imap_uid=? AND uid_validity=?`, uid, uidValidity, strings.Join(flags, " "), seen, time.Now().UTC().Format(time.RFC3339Nano), accountID, folder, uid, uidValidity)
	return err
}

// PruneRemoteFolders removes folder records no longer returned by a complete
// IMAP discovery pass. Indexed messages are reconciled separately by the
// Maildir indexer, so an empty deleted folder cannot remain in the phone menu.
func (s *Store) PruneRemoteFolders(ctx context.Context, accountID string, remotePaths []string) error {
	if accountID == "" {
		return fmt.Errorf("account identity is required")
	}
	if len(remotePaths) == 0 {
		_, err := s.DB.ExecContext(ctx, `DELETE FROM remote_folders WHERE account_id=?`, accountID)
		return err
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(remotePaths)), ",")
	args := make([]any, 0, len(remotePaths)+1)
	args = append(args, accountID)
	for _, path := range remotePaths {
		args = append(args, path)
	}
	_, err := s.DB.ExecContext(ctx, `DELETE FROM remote_folders WHERE account_id=? AND remote_path NOT IN (`+placeholders+`)`, args...)
	return err
}

const imapSeenFlag = "\\Seen"

func (s *Store) ReplaceAttachmentMetadata(ctx context.Context, messageID int64, attachments []AttachmentMetadata) error {
	if messageID == 0 {
		return fmt.Errorf("message identity is required")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM attachment_metadata WHERE message_id=?`, messageID); err != nil {
		return err
	}
	for _, attachment := range attachments {
		if strings.TrimSpace(attachment.PartPath) == "" || strings.TrimSpace(attachment.Filename) == "" {
			return fmt.Errorf("attachment metadata identity is required")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO attachment_metadata(message_id,part_path,filename,content_type,size,content_id,disposition,playable) VALUES(?,?,?,?,?,?,?,?)`, messageID, attachment.PartPath, attachment.Filename, attachment.ContentType, attachment.Size, attachment.ContentID, attachment.Disposition, attachment.Playable); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) UpsertRemoteFolder(ctx context.Context, accountID, remotePath, localPath, role string) error {
	remotePath = strings.TrimSpace(remotePath)
	localPath = filepathCleanFolder(localPath)
	if accountID == "" || remotePath == "" {
		return fmt.Errorf("remote folder identity is required")
	}
	parent := ""
	if slash := strings.LastIndex(strings.ReplaceAll(remotePath, "\\", "/"), "/"); slash > 0 {
		parent = remotePath[:slash]
	}
	depth := 0
	if parent != "" {
		depth = strings.Count(strings.ReplaceAll(remotePath, "\\", "/"), "/")
	}
	display := remotePath
	if slash := strings.LastIndex(strings.ReplaceAll(remotePath, "\\", "/"), "/"); slash >= 0 && slash+1 < len(remotePath) {
		display = remotePath[slash+1:]
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO remote_folders(account_id,remote_path,local_path,parent_path,depth,role,display_name,updated_at) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(account_id,remote_path) DO UPDATE SET local_path=excluded.local_path,parent_path=excluded.parent_path,depth=excluded.depth,role=excluded.role,display_name=excluded.display_name,updated_at=excluded.updated_at`, accountID, remotePath, localPath, parent, depth, role, display, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) DeleteMailIndex(ctx context.Context, userID string, id int64) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM mail_messages WHERE id=? AND account_id IN (SELECT id FROM accounts WHERE user_id=?)`, id, userID)
	return err
}

func (s *Store) RecordMessageMutation(ctx context.Context, messageID int64, userID, operation, fromFolder, toFolder, status string, mutationErr error) error {
	if messageID == 0 || userID == "" || operation == "" || status == "" {
		return fmt.Errorf("message mutation identity is required")
	}
	errText := ""
	if mutationErr != nil {
		errText = mutationErr.Error()
	}
	result, err := s.DB.ExecContext(ctx, `
INSERT INTO message_mutations(message_id,user_id,operation,from_folder,to_folder,status,error,created_at)
SELECT m.id, a.user_id, ?, ?, ?, ?, ?, ?
FROM mail_messages m JOIN accounts a ON a.id=m.account_id
WHERE m.id=? AND a.user_id=?`, operation, fromFolder, toFolder, status, errText, time.Now().UTC().Format(time.RFC3339Nano), messageID, userID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("message was not found for user")
	}
	return nil
}

// MarkQueuedMutationsSynchronized closes the retry/reporting loop for local
// Maildir-first flag changes. A successful mbsync run is the confirmation;
// failed runs leave queued rows untouched so the next scheduled run retries
// them naturally.
func (s *Store) MarkQueuedMutationsSynchronized(ctx context.Context, accountID string) error {
	if strings.TrimSpace(accountID) == "" {
		return fmt.Errorf("account identity is required")
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE message_mutations SET status='synchronized',error='' WHERE status='queued' AND message_id IN (SELECT id FROM mail_messages WHERE account_id=?)`, accountID)
	return err
}

func (s *Store) ListMessageMutations(ctx context.Context, userID, accountID string, limit int) ([]MessageMutation, error) {
	if strings.TrimSpace(userID) == "" {
		return nil, fmt.Errorf("user identity is required")
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	query := `SELECT m.id,m.message_id,mm.account_id,m.operation,m.from_folder,m.to_folder,m.status,m.error,m.created_at
		FROM message_mutations m JOIN mail_messages mm ON mm.id=m.message_id JOIN accounts a ON a.id=mm.account_id
		WHERE m.user_id=? AND a.user_id=?`
	args := []any{userID, userID}
	if strings.TrimSpace(accountID) != "" {
		query += ` AND mm.account_id=?`
		args = append(args, accountID)
	}
	query += ` ORDER BY m.id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MessageMutation
	for rows.Next() {
		var item MessageMutation
		if err := rows.Scan(&item.ID, &item.MessageID, &item.AccountID, &item.Operation, &item.FromFolder, &item.ToFolder, &item.Status, &item.Error, &item.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// AlertCandidate is an unseen, unsent message in an alert-enabled account.
// Phone is the normalized alert number and Voice the user's TTS voice; the
// alerts service rounds up candidates per user and dials the number.
type AlertCandidate struct {
	MessageID    int64
	AccountID    string
	Folder       string
	Sender       string
	Subject      string
	UserID       string
	Phone        string
	Voice        string
	AlertFolders string
	Date         string
}

// PendingAlerts returns unread messages that have not been announced by a
// phone alert and belong to accounts with call alerts enabled whose owner has
// an enabled alerts setting and a number on file.
func (s *Store) PendingAlerts(ctx context.Context) ([]AlertCandidate, error) {
	available, err := s.AlertsAvailable(ctx)
	if err != nil {
		return nil, err
	}
	if !available {
		return []AlertCandidate{}, nil
	}
	staleBefore := time.Now().UTC().Add(-15 * time.Minute).Format(time.RFC3339Nano)
	if _, err := s.DB.ExecContext(ctx, `DELETE FROM alert_claims WHERE announced_at IS NULL AND claimed_at < ?`, staleBefore); err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT m.id,m.account_id,m.folder,COALESCE(m.sender,''),COALESCE(m.subject,''),u.id,COALESCE(n.number,s.alert_phone,''),s.tts_voice,COALESCE(a.alert_folders,'[]'),COALESCE(m.message_date,'')
FROM mail_messages m
JOIN accounts a ON a.id = m.account_id
JOIN users u ON u.id = a.user_id
JOIN settings s ON s.user_id = u.id
LEFT JOIN alert_numbers n ON n.user_id=u.id AND n.active=1
WHERE m.is_read = 0 AND m.alerted = 0
  AND a.call_alert_enabled = 1 AND s.alerts_enabled = 1 AND u.enabled = 1
  AND COALESCE(n.number,s.alert_phone,'') <> '' AND COALESCE(a.alert_folders,'[]') <> '[]'
  AND NOT EXISTS (SELECT 1 FROM alert_claims c WHERE c.message_id=m.id AND c.announced_at IS NULL)
ORDER BY COALESCE(m.message_date,'') DESC, m.id DESC
LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AlertCandidate
	for rows.Next() {
		var c AlertCandidate
		if err := rows.Scan(&c.MessageID, &c.AccountID, &c.Folder, &c.Sender, &c.Subject, &c.UserID, &c.Phone, &c.Voice, &c.AlertFolders, &c.Date); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ClaimAlertMessages atomically reserves eligible messages for one user's
// outbound call. Claims survive process restarts; stale unannounced claims
// are released when candidates are read or claimed again.
func (s *Store) ClaimAlertMessages(ctx context.Context, userID string, ids []int64) ([]int64, error) {
	if userID == "" || len(ids) == 0 {
		return nil, nil
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	cutoff := time.Now().UTC().Add(-15 * time.Minute).Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `DELETE FROM alert_claims WHERE announced_at IS NULL AND claimed_at < ?`, cutoff); err != nil {
		return nil, err
	}
	claimedAt := time.Now().UTC().Format(time.RFC3339Nano)
	claimed := make([]int64, 0, len(ids))
	for _, id := range ids {
		result, err := tx.ExecContext(ctx, `
INSERT OR IGNORE INTO alert_claims(message_id,user_id,claimed_at)
SELECT m.id,a.user_id,? FROM mail_messages m JOIN accounts a ON a.id=m.account_id
WHERE m.id=? AND a.user_id=? AND m.is_read=0 AND m.alerted=0`, claimedAt, id, userID)
		if err != nil {
			return nil, err
		}
		if count, _ := result.RowsAffected(); count == 1 {
			claimed = append(claimed, id)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return claimed, nil
}

func (s *Store) ReleaseAlertClaims(ctx context.Context, userID string, ids []int64) error {
	if userID == "" || len(ids) == 0 {
		return nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids)+1)
	args = append(args, userID)
	args = append(args, idsToAny(ids)...)
	_, err := s.DB.ExecContext(ctx, `DELETE FROM alert_claims WHERE user_id=? AND announced_at IS NULL AND message_id IN (`+placeholders+`)`, args...)
	return err
}

func (s *Store) MarkAlertMessagesNotified(ctx context.Context, userID string, ids []int64) error {
	if userID == "" || len(ids) == 0 {
		return nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	messageArgs := idsToAny(ids)
	messageArgs = append(messageArgs, userID)
	if _, err := tx.ExecContext(ctx, `UPDATE mail_messages SET alerted=1 WHERE id IN (`+placeholders+`) AND account_id IN (SELECT id FROM accounts WHERE user_id=?)`, messageArgs...); err != nil {
		return err
	}
	claimArgs := []any{time.Now().UTC().Format(time.RFC3339Nano), userID}
	claimArgs = append(claimArgs, idsToAny(ids)...)
	if _, err := tx.ExecContext(ctx, `UPDATE alert_claims SET announced_at=? WHERE user_id=? AND message_id IN (`+placeholders+`)`, claimArgs...); err != nil {
		return err
	}
	return tx.Commit()
}

func idsToAny(ids []int64) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}

// MarkAlertsNotified claims candidates so a later sync cannot announce the
// same message a second time.
func (s *Store) MarkAlertsNotified(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE mail_messages SET alerted = 1 WHERE id IN (`+placeholders+`)`, args...)
	return err
}

func (s *Store) UserCount(ctx context.Context) (int, error) {
	var count int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count)
	return count, err
}

func (s *Store) CreateUser(ctx context.Context, user User) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO users(id, username, password_hash, pin_hash, role, enabled, totp_secret, backup_codes, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, user.ID, user.Username, user.PasswordHash, user.PINHash, user.Role, user.Enabled, user.TOTPSecret, user.BackupCodes, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings(user_id) VALUES (?)`, user.ID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) UserByUsername(ctx context.Context, username string) (User, error) {
	var user User
	var enabled int
	err := s.DB.QueryRowContext(ctx, `SELECT id, username, password_hash, pin_hash, role, enabled, COALESCE(totp_secret,''), COALESCE(backup_codes,'') FROM users WHERE username = ?`, username).Scan(&user.ID, &user.Username, &user.PasswordHash, &user.PINHash, &user.Role, &enabled, &user.TOTPSecret, &user.BackupCodes)
	user.Enabled = enabled != 0
	return user, err
}

func (s *Store) UserByID(ctx context.Context, id string) (User, error) {
	var user User
	var enabled int
	err := s.DB.QueryRowContext(ctx, `SELECT id, username, password_hash, pin_hash, role, enabled, COALESCE(totp_secret,''), COALESCE(backup_codes,'') FROM users WHERE id = ?`, id).Scan(&user.ID, &user.Username, &user.PasswordHash, &user.PINHash, &user.Role, &enabled, &user.TOTPSecret, &user.BackupCodes)
	user.Enabled = enabled != 0
	return user, err
}

func (s *Store) UserByPhone(ctx context.Context, phone string) (User, error) {
	var user User
	var enabled int
	err := s.DB.QueryRowContext(ctx, `SELECT u.id,u.username,u.password_hash,u.pin_hash,u.role,u.enabled,COALESCE(u.totp_secret,''),COALESCE(u.backup_codes,'') FROM users u JOIN caller_whitelist w ON w.user_id=u.id WHERE w.phone=?`, phone).Scan(&user.ID, &user.Username, &user.PasswordHash, &user.PINHash, &user.Role, &enabled, &user.TOTPSecret, &user.BackupCodes)
	user.Enabled = enabled != 0
	return user, err
}

func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, username, password_hash, pin_hash, role, enabled, COALESCE(totp_secret,''), COALESCE(backup_codes,'') FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []User
	for rows.Next() {
		var user User
		var enabled int
		if err := rows.Scan(&user.ID, &user.Username, &user.PasswordHash, &user.PINHash, &user.Role, &enabled, &user.TOTPSecret, &user.BackupCodes); err != nil {
			return nil, err
		}
		user.Enabled = enabled != 0
		users = append(users, user)
	}
	return users, rows.Err()
}

func (s *Store) DeleteUser(ctx context.Context, id string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
	return err
}

func (s *Store) AddWhitelist(ctx context.Context, entry WhitelistEntry) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO caller_whitelist(user_id, phone, created_at) VALUES (?, ?, ?)`, entry.UserID, entry.Phone, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) ListWhitelist(ctx context.Context, userID string) ([]WhitelistEntry, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, user_id, phone FROM caller_whitelist WHERE user_id = ? ORDER BY phone`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []WhitelistEntry
	for rows.Next() {
		var entry WhitelistEntry
		if err := rows.Scan(&entry.ID, &entry.UserID, &entry.Phone); err != nil {
			return nil, err
		}
		result = append(result, entry)
	}
	return result, rows.Err()
}

func (s *Store) DeleteWhitelist(ctx context.Context, userID string, id int64) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM caller_whitelist WHERE user_id = ? AND id = ?`, userID, id)
	return err
}

func (s *Store) ListAlertNumbers(ctx context.Context, userID string) ([]AlertNumber, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,user_id,number,active,created_at FROM alert_numbers WHERE user_id=? ORDER BY active DESC, id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []AlertNumber
	for rows.Next() {
		var number AlertNumber
		var active int
		if err := rows.Scan(&number.ID, &number.UserID, &number.Number, &active, &number.CreatedAt); err != nil {
			return nil, err
		}
		number.Active = active != 0
		result = append(result, number)
	}
	return result, rows.Err()
}

func (s *Store) SaveAlertNumber(ctx context.Context, userID, number string, active bool) error {
	userID, number = strings.TrimSpace(userID), strings.TrimSpace(number)
	if userID == "" || number == "" {
		return fmt.Errorf("user and alert number are required")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if active {
		if _, err := tx.ExecContext(ctx, `UPDATE alert_numbers SET active=0 WHERE user_id=?`, userID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO alert_numbers(user_id,number,active,created_at) VALUES(?,?,?,?) ON CONFLICT(user_id,number) DO UPDATE SET active=excluded.active`, userID, number, active, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM alert_numbers WHERE user_id=? AND active=1`, userID).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE alert_numbers SET active=1 WHERE id=(SELECT id FROM alert_numbers WHERE user_id=? ORDER BY id LIMIT 1)`, userID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) SetActiveAlertNumber(ctx context.Context, userID string, id int64) error {
	if userID == "" || id == 0 {
		return fmt.Errorf("user and alert number are required")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var found int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM alert_numbers WHERE id=? AND user_id=?`, id, userID).Scan(&found); err != nil {
		return err
	}
	if found != 1 {
		return sql.ErrNoRows
	}
	if _, err := tx.ExecContext(ctx, `UPDATE alert_numbers SET active=0 WHERE user_id=?`, userID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE alert_numbers SET active=1 WHERE id=? AND user_id=?`, id, userID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) DeleteAlertNumber(ctx context.Context, userID string, id int64) error {
	if userID == "" || id == 0 {
		return fmt.Errorf("user and alert number are required")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT active FROM alert_numbers WHERE id=? AND user_id=?`, id, userID).Scan(&active); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM alert_numbers WHERE id=? AND user_id=?`, id, userID); err != nil {
		return err
	}
	if active != 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE alert_numbers SET active=1 WHERE id=(SELECT id FROM alert_numbers WHERE user_id=? ORDER BY id LIMIT 1)`, userID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ClearAlertNumbers(ctx context.Context, userID string) error {
	if strings.TrimSpace(userID) == "" {
		return fmt.Errorf("user is required")
	}
	_, err := s.DB.ExecContext(ctx, `DELETE FROM alert_numbers WHERE user_id=?`, userID)
	return err
}

func (s *Store) ActiveAlertNumber(ctx context.Context, userID string) (string, error) {
	var number string
	err := s.DB.QueryRowContext(ctx, `SELECT number FROM alert_numbers WHERE user_id=? AND active=1 LIMIT 1`, userID).Scan(&number)
	if err == nil {
		return number, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	err = s.DB.QueryRowContext(ctx, `SELECT COALESCE(alert_phone,'') FROM settings WHERE user_id=?`, userID).Scan(&number)
	return strings.TrimSpace(number), err
}

func (s *Store) SaveAccount(ctx context.Context, box *secret.Box, account Account) error {
	if box == nil || account.ID == "" || account.UserID == "" {
		return fmt.Errorf("secret box and account ownership are required")
	}
	imapSecurity, err := securityMode(account.IMAPSecurity, account.IMAPPort)
	if err != nil {
		return fmt.Errorf("IMAP security: %w", err)
	}
	smtpSecurity, err := securityMode(account.SMTPSecurity, account.SMTPPort)
	if err != nil {
		return fmt.Errorf("SMTP security: %w", err)
	}
	var owner string
	if err := s.DB.QueryRowContext(ctx, `SELECT user_id FROM accounts WHERE id = ?`, account.ID).Scan(&owner); err == nil && owner != account.UserID {
		return fmt.Errorf("account belongs to another user")
	}
	var existingIMAP, existingSMTP string
	if account.IMAPPassword == "" || account.SMTPPassword == "" {
		_ = s.DB.QueryRowContext(ctx, `SELECT imap_password, smtp_password FROM accounts WHERE id = ? AND user_id = ?`, account.ID, account.UserID).Scan(&existingIMAP, &existingSMTP)
	}
	var imapPass, smtpPass string
	if account.IMAPPassword == "" && existingIMAP != "" {
		imapPass = existingIMAP
	} else {
		imapPass, err = box.Seal(account.IMAPPassword)
	}
	if err != nil {
		return err
	}
	if account.SMTPPassword == "" && existingSMTP != "" {
		smtpPass = existingSMTP
	} else {
		smtpPass, err = box.Seal(account.SMTPPassword)
	}
	if err != nil {
		return err
	}
	alertFolders := strings.TrimSpace(account.AlertFolders)
	var folderMap map[string]string
	_ = json.Unmarshal([]byte(account.FolderMap), &folderMap)
	var selectedFolders []string
	if json.Unmarshal([]byte(alertFolders), &selectedFolders) != nil {
		selectedFolders = nil
	}
	if len(selectedFolders) == 0 && account.CallAlertEnabled {
		selectedFolders = []string{mappedInboxFolder(folderMap)}
	} else {
		for i, folder := range selectedFolders {
			for remote, local := range folderMap {
				if strings.EqualFold(strings.TrimSpace(folder), strings.TrimSpace(remote)) && local != "" {
					selectedFolders[i] = local
					break
				}
			}
		}
	}
	if encoded, marshalErr := json.Marshal(selectedFolders); marshalErr == nil {
		alertFolders = string(encoded)
	}
	_, err = s.DB.ExecContext(ctx, `INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_port,imap_user,imap_password,imap_security,smtp_host,smtp_port,smtp_security,smtp_user,smtp_password,folder_map,sync_interval_minutes,reconciliation_interval_minutes,initial_cutoff,retention_days,call_alert_enabled,alert_folders,display_order,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET canonical_name=excluded.canonical_name,email=excluded.email,sender_name=excluded.sender_name,imap_host=excluded.imap_host,imap_port=excluded.imap_port,imap_user=excluded.imap_user,imap_password=excluded.imap_password,imap_security=excluded.imap_security,smtp_host=excluded.smtp_host,smtp_port=excluded.smtp_port,smtp_security=excluded.smtp_security,smtp_user=excluded.smtp_user,smtp_password=excluded.smtp_password,folder_map=excluded.folder_map,sync_interval_minutes=excluded.sync_interval_minutes,reconciliation_interval_minutes=excluded.reconciliation_interval_minutes,initial_cutoff=excluded.initial_cutoff,retention_days=excluded.retention_days,call_alert_enabled=excluded.call_alert_enabled,alert_folders=excluded.alert_folders,display_order=excluded.display_order`, account.ID, account.UserID, account.CanonicalName, account.Email, account.SenderName, account.IMAPHost, account.IMAPPort, account.IMAPUser, imapPass, imapSecurity, account.SMTPHost, account.SMTPPort, smtpSecurity, account.SMTPUser, smtpPass, account.FolderMap, account.SyncIntervalMinutes, account.ReconciliationIntervalMinutes, account.InitialCutoff, account.RetentionDays, account.CallAlertEnabled, alertFolders, account.DisplayOrder, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func mappedInboxFolder(mapping map[string]string) string {
	for remote, local := range mapping {
		if strings.EqualFold(strings.TrimSpace(remote), "INBOX") && strings.TrimSpace(local) != "" {
			return strings.TrimSpace(local)
		}
	}
	return "Inbox"
}

func securityMode(value string, port int) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		if port == 465 || port == 993 {
			return "implicit_tls", nil
		}
		return "starttls", nil
	}
	if value != "implicit_tls" && value != "starttls" && value != "plaintext" {
		return "", fmt.Errorf("must be implicit_tls, starttls, or plaintext")
	}
	if value == "plaintext" {
		return "", fmt.Errorf("plaintext transport is not permitted")
	}
	return value, nil
}

func (s *Store) ListAccounts(ctx context.Context, userID string) ([]Account, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,user_id,canonical_name,email,sender_name,imap_host,imap_port,imap_user,imap_password,imap_security,smtp_host,smtp_port,smtp_security,smtp_user,smtp_password,folder_map,sync_interval_minutes,reconciliation_interval_minutes,initial_cutoff,retention_days,call_alert_enabled,alert_folders,display_order FROM accounts WHERE user_id = ? ORDER BY display_order, canonical_name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Account
	for rows.Next() {
		var a Account
		var alerts int
		if err := rows.Scan(&a.ID, &a.UserID, &a.CanonicalName, &a.Email, &a.SenderName, &a.IMAPHost, &a.IMAPPort, &a.IMAPUser, &a.IMAPPassword, &a.IMAPSecurity, &a.SMTPHost, &a.SMTPPort, &a.SMTPSecurity, &a.SMTPUser, &a.SMTPPassword, &a.FolderMap, &a.SyncIntervalMinutes, &a.ReconciliationIntervalMinutes, &a.InitialCutoff, &a.RetentionDays, &alerts, &a.AlertFolders, &a.DisplayOrder); err != nil {
			return nil, err
		}
		a.CallAlertEnabled = alerts != 0
		result = append(result, a)
	}
	return result, rows.Err()
}

func (s *Store) ListAllAccounts(ctx context.Context) ([]Account, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,user_id,canonical_name,email,sender_name,imap_host,imap_port,imap_user,imap_password,imap_security,smtp_host,smtp_port,smtp_security,smtp_user,smtp_password,folder_map,sync_interval_minutes,reconciliation_interval_minutes,initial_cutoff,retention_days,call_alert_enabled,alert_folders,display_order FROM accounts ORDER BY user_id, display_order, canonical_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Account
	for rows.Next() {
		var a Account
		var alerts int
		if err := rows.Scan(&a.ID, &a.UserID, &a.CanonicalName, &a.Email, &a.SenderName, &a.IMAPHost, &a.IMAPPort, &a.IMAPUser, &a.IMAPPassword, &a.IMAPSecurity, &a.SMTPHost, &a.SMTPPort, &a.SMTPSecurity, &a.SMTPUser, &a.SMTPPassword, &a.FolderMap, &a.SyncIntervalMinutes, &a.ReconciliationIntervalMinutes, &a.InitialCutoff, &a.RetentionDays, &alerts, &a.AlertFolders, &a.DisplayOrder); err != nil {
			return nil, err
		}
		a.CallAlertEnabled = alerts != 0
		result = append(result, a)
	}
	return result, rows.Err()
}

func (s *Store) DeleteAccount(ctx context.Context, userID, id string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM accounts WHERE user_id = ? AND id = ?`, userID, id)
	return err
}

// BeginSyncRun records a sync before any external work starts. This makes a
// running or interrupted sync visible after a process restart.
func (s *Store) BeginSyncRun(ctx context.Context, accountID, kind string) (int64, error) {
	if strings.TrimSpace(kind) == "" {
		kind = "incremental"
	}
	res, err := s.DB.ExecContext(ctx, `INSERT INTO sync_runs(account_id,kind,started_at,success,changed,error) VALUES(?,?,?,0,0,'')`, accountID, kind, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) FinishSyncRun(ctx context.Context, id int64, success, changed bool, runErr error) error {
	if id == 0 {
		return nil
	}
	errText := ""
	if runErr != nil {
		errText = runErr.Error()
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE sync_runs SET finished_at=?,success=?,changed=?,error=? WHERE id=?`, time.Now().UTC().Format(time.RFC3339Nano), success, changed, errText, id)
	return err
}

func (s *Store) LatestSyncRun(ctx context.Context, accountID string, successfulOnly bool) (SyncRun, error) {
	query := `SELECT id,account_id,kind,started_at,COALESCE(finished_at,''),success,changed,error FROM sync_runs WHERE account_id=?`
	if successfulOnly {
		query += ` AND success=1`
	}
	query += ` ORDER BY id DESC LIMIT 1`
	var run SyncRun
	var success, changed int
	err := s.DB.QueryRowContext(ctx, query, accountID).Scan(&run.ID, &run.AccountID, &run.Kind, &run.StartedAt, &run.FinishedAt, &success, &changed, &run.Error)
	if err != nil {
		return SyncRun{}, err
	}
	run.Success = success != 0
	run.Changed = changed != 0
	return run, nil
}

func (s *Store) ListSyncRuns(ctx context.Context, userID, accountID string, limit int) ([]SyncRun, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	query := `SELECT r.id,r.account_id,r.kind,r.started_at,COALESCE(r.finished_at,''),r.success,r.changed,r.error FROM sync_runs r JOIN accounts a ON a.id=r.account_id WHERE a.user_id=?`
	args := []any{userID}
	if accountID != "" {
		query += ` AND r.account_id=?`
		args = append(args, accountID)
	}
	query += ` ORDER BY r.id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SyncRun
	for rows.Next() {
		var run SyncRun
		var success, changed int
		if err := rows.Scan(&run.ID, &run.AccountID, &run.Kind, &run.StartedAt, &run.FinishedAt, &success, &changed, &run.Error); err != nil {
			return nil, err
		}
		run.Success = success != 0
		run.Changed = changed != 0
		out = append(out, run)
	}
	return out, rows.Err()
}

func (s *Store) AddContact(ctx context.Context, contact Contact) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `INSERT INTO contacts(user_id,name,email,display_order) VALUES (?,?,?,?)`, contact.UserID, contact.Name, contact.Email, contact.DisplayOrder)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}
func (s *Store) UpdateContact(ctx context.Context, contact Contact) error {
	res, err := s.DB.ExecContext(ctx, `UPDATE contacts SET name=?, email=?, display_order=? WHERE id=? AND user_id=?`, contact.Name, contact.Email, contact.DisplayOrder, contact.ID, contact.UserID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
func (s *Store) ListContacts(ctx context.Context, userID string) ([]Contact, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,user_id,name,email,display_order FROM contacts WHERE user_id=? ORDER BY display_order,name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Contact
	for rows.Next() {
		var c Contact
		if err := rows.Scan(&c.ID, &c.UserID, &c.Name, &c.Email, &c.DisplayOrder); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
func (s *Store) DeleteContact(ctx context.Context, userID string, id int64) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM contacts WHERE user_id=? AND id=?`, userID, id)
	return err
}

func (s *Store) GetSIP(ctx context.Context) (SIPSettings, error) {
	var st SIPSettings
	var enabled int
	err := s.DB.QueryRowContext(ctx, `SELECT domain,username,password,port,transport,reg_interval,enabled FROM sip_settings WHERE id=1`).Scan(&st.Domain, &st.Username, &st.Password, &st.Port, &st.Transport, &st.RegInterval, &enabled)
	if err != nil {
		return SIPSettings{}, err
	}
	st.Enabled = enabled != 0
	return st, nil
}

// UpsertSIP persists the deployment SIP account. When storage.Password is
// blank the previously sealed password is preserved; otherwise the plaintext
// is sealed before writing.
func (s *Store) UpsertSIP(ctx context.Context, box *secret.Box, storage SIPSettings) error {
	if box == nil {
		return fmt.Errorf("secret box is required")
	}
	if storage.Port == 0 {
		storage.Port = 5060
	}
	if storage.Transport == "" {
		storage.Transport = "udp"
	}
	if storage.RegInterval == 0 {
		storage.RegInterval = 300
	}
	sealed := storage.Password
	if sealed == "" {
		_ = s.DB.QueryRowContext(ctx, `SELECT password FROM sip_settings WHERE id=1`).Scan(&sealed)
	}
	if storage.Password != "" {
		value, err := box.Seal(storage.Password)
		if err != nil {
			return err
		}
		sealed = value
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO sip_settings(id,domain,username,password,port,transport,reg_interval,enabled,updated_at) VALUES(1,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET domain=excluded.domain,username=excluded.username,password=excluded.password,port=excluded.port,transport=excluded.transport,reg_interval=excluded.reg_interval,enabled=excluded.enabled,updated_at=excluded.updated_at`,
		storage.Domain, storage.Username, sealed, storage.Port, storage.Transport, storage.RegInterval, storage.Enabled, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}
