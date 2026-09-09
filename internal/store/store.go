package store

import (
	"context"
	"database/sql"
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
 imap_password TEXT NOT NULL, smtp_host TEXT NOT NULL, smtp_port INTEGER NOT NULL DEFAULT 465,
 smtp_user TEXT NOT NULL, smtp_password TEXT NOT NULL, folder_map TEXT NOT NULL DEFAULT '{}',
 sync_interval_minutes INTEGER NOT NULL DEFAULT 5, initial_cutoff TEXT, retention_days INTEGER,
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
 recipients TEXT, subject TEXT, message_date TEXT, is_read INTEGER NOT NULL DEFAULT 0,
 attachment_count INTEGER NOT NULL DEFAULT 0, alerted INTEGER NOT NULL DEFAULT 0,
 updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS mail_alert_idx ON mail_messages(account_id, folder, is_read, alerted);`)
	if err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	if err := s.ensureColumn(ctx, "mail_messages", "alerted", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if _, err := s.DB.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS mail_alert_idx ON mail_messages(account_id, folder, is_read, alerted)`); err != nil {
		return fmt.Errorf("migrate alter table: %w", err)
	}
	return nil
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

func (s *Store) Audit(ctx context.Context, userID, action, detail string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO audit_log(user_id, action, detail, created_at) VALUES (?, ?, ?, ?)`, userID, action, detail, time.Now().UTC().Format(time.RFC3339Nano))
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
	ID                  string  `json:"id"`
	UserID              string  `json:"user_id"`
	CanonicalName       string  `json:"canonical_name"`
	Email               string  `json:"email"`
	SenderName          string  `json:"sender_name"`
	IMAPHost            string  `json:"imap_host"`
	IMAPUser            string  `json:"imap_user"`
	SMTPHost            string  `json:"smtp_host"`
	SMTPUser            string  `json:"smtp_user"`
	IMAPPort            int     `json:"imap_port"`
	SMTPPort            int     `json:"smtp_port"`
	IMAPPassword        string  `json:"-"`
	SMTPPassword        string  `json:"-"`
	FolderMap           string  `json:"folder_map"`
	AlertFolders        string  `json:"alert_folders"`
	SyncIntervalMinutes int     `json:"sync_interval_minutes"`
	DisplayOrder        int     `json:"display_order"`
	InitialCutoff       *string `json:"initial_cutoff,omitempty"`
	RetentionDays       *int    `json:"retention_days,omitempty"`
	CallAlertEnabled    bool    `json:"call_alert_enabled"`
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
	ID                                                                    int64
	AccountID, Folder, Path, MessageID, Sender, Recipients, Subject, Date string
	Read                                                                  bool
	Attachments                                                           int
}

func (s *Store) ListMail(ctx context.Context, userID string, unreadOnly bool) ([]MailSummary, error) {
	query := `SELECT m.id,m.account_id,m.folder,m.path,COALESCE(m.message_id,''),COALESCE(m.sender,''),COALESCE(m.recipients,''),COALESCE(m.subject,''),COALESCE(m.message_date,''),m.is_read,m.attachment_count FROM mail_messages m JOIN accounts a ON a.id=m.account_id WHERE a.user_id=?`
	if unreadOnly {
		query += ` AND m.is_read=0`
	}
	query += ` ORDER BY COALESCE(m.message_date,''),m.id DESC`
	rows, err := s.DB.QueryContext(ctx, query, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MailSummary
	for rows.Next() {
		var m MailSummary
		var read int
		if err := rows.Scan(&m.ID, &m.AccountID, &m.Folder, &m.Path, &m.MessageID, &m.Sender, &m.Recipients, &m.Subject, &m.Date, &read, &m.Attachments); err != nil {
			return nil, err
		}
		m.Read = read != 0
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) ListMailForAccount(ctx context.Context, userID, accountID, folder string, unreadOnly bool) ([]MailSummary, error) {
	query := `SELECT m.id,m.account_id,m.folder,m.path,COALESCE(m.message_id,''),COALESCE(m.sender,''),COALESCE(m.recipients,''),COALESCE(m.subject,''),COALESCE(m.message_date,''),m.is_read,m.attachment_count FROM mail_messages m JOIN accounts a ON a.id=m.account_id WHERE a.user_id=? AND m.account_id=?`
	args := []any{userID, accountID}
	if folder != "" {
		query += ` AND m.folder=?`
		args = append(args, folder)
	}
	if unreadOnly {
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
		if err := rows.Scan(&m.ID, &m.AccountID, &m.Folder, &m.Path, &m.MessageID, &m.Sender, &m.Recipients, &m.Subject, &m.Date, &read, &m.Attachments); err != nil {
			return nil, err
		}
		m.Read = read != 0
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) ListMailFolders(ctx context.Context, userID, accountID string) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT DISTINCT m.folder FROM mail_messages m JOIN accounts a ON a.id=m.account_id WHERE a.user_id=? AND m.account_id=? ORDER BY m.folder`, userID, accountID)
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
	_, err := s.DB.ExecContext(ctx, `UPDATE mail_messages SET is_read=? WHERE id=? AND account_id IN (SELECT id FROM accounts WHERE user_id=?)`, read, id, userID)
	return err
}

func (s *Store) DeleteMailIndex(ctx context.Context, userID string, id int64) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM mail_messages WHERE id=? AND account_id IN (SELECT id FROM accounts WHERE user_id=?)`, id, userID)
	return err
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
	rows, err := s.DB.QueryContext(ctx, `SELECT m.id,m.account_id,m.folder,COALESCE(m.sender,''),COALESCE(m.subject,''),u.id,COALESCE(s.alert_phone,''),s.tts_voice,COALESCE(a.alert_folders,'[]'),COALESCE(m.message_date,'')
FROM mail_messages m
JOIN accounts a ON a.id = m.account_id
JOIN users u ON u.id = a.user_id
JOIN settings s ON s.user_id = u.id
WHERE m.is_read = 0 AND m.alerted = 0
  AND a.call_alert_enabled = 1 AND s.alerts_enabled = 1 AND u.enabled = 1
  AND COALESCE(s.alert_phone,'') <> '' AND COALESCE(a.alert_folders,'[]') <> '[]'
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
	_, err := s.DB.ExecContext(ctx, `INSERT INTO users(id, username, password_hash, pin_hash, role, enabled, totp_secret, backup_codes, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, user.ID, user.Username, user.PasswordHash, user.PINHash, user.Role, user.Enabled, user.TOTPSecret, user.BackupCodes, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx, `INSERT INTO settings(user_id) VALUES (?)`, user.ID)
	return err
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

func (s *Store) SaveAccount(ctx context.Context, box *secret.Box, account Account) error {
	if box == nil || account.ID == "" || account.UserID == "" {
		return fmt.Errorf("secret box and account ownership are required")
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
	var err error
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
	_, err = s.DB.ExecContext(ctx, `INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_port,imap_user,imap_password,smtp_host,smtp_port,smtp_user,smtp_password,folder_map,sync_interval_minutes,initial_cutoff,retention_days,call_alert_enabled,alert_folders,display_order,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET canonical_name=excluded.canonical_name,email=excluded.email,sender_name=excluded.sender_name,imap_host=excluded.imap_host,imap_port=excluded.imap_port,imap_user=excluded.imap_user,imap_password=excluded.imap_password,smtp_host=excluded.smtp_host,smtp_port=excluded.smtp_port,smtp_user=excluded.smtp_user,smtp_password=excluded.smtp_password,folder_map=excluded.folder_map,sync_interval_minutes=excluded.sync_interval_minutes,initial_cutoff=excluded.initial_cutoff,retention_days=excluded.retention_days,call_alert_enabled=excluded.call_alert_enabled,alert_folders=excluded.alert_folders,display_order=excluded.display_order`, account.ID, account.UserID, account.CanonicalName, account.Email, account.SenderName, account.IMAPHost, account.IMAPPort, account.IMAPUser, imapPass, account.SMTPHost, account.SMTPPort, account.SMTPUser, smtpPass, account.FolderMap, account.SyncIntervalMinutes, account.InitialCutoff, account.RetentionDays, account.CallAlertEnabled, account.AlertFolders, account.DisplayOrder, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) ListAccounts(ctx context.Context, userID string) ([]Account, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,user_id,canonical_name,email,sender_name,imap_host,imap_port,imap_user,imap_password,smtp_host,smtp_port,smtp_user,smtp_password,folder_map,sync_interval_minutes,initial_cutoff,retention_days,call_alert_enabled,alert_folders,display_order FROM accounts WHERE user_id = ? ORDER BY display_order, canonical_name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Account
	for rows.Next() {
		var a Account
		var alerts int
		if err := rows.Scan(&a.ID, &a.UserID, &a.CanonicalName, &a.Email, &a.SenderName, &a.IMAPHost, &a.IMAPPort, &a.IMAPUser, &a.IMAPPassword, &a.SMTPHost, &a.SMTPPort, &a.SMTPUser, &a.SMTPPassword, &a.FolderMap, &a.SyncIntervalMinutes, &a.InitialCutoff, &a.RetentionDays, &alerts, &a.AlertFolders, &a.DisplayOrder); err != nil {
			return nil, err
		}
		a.CallAlertEnabled = alerts != 0
		result = append(result, a)
	}
	return result, rows.Err()
}

func (s *Store) ListAllAccounts(ctx context.Context) ([]Account, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,user_id,canonical_name,email,sender_name,imap_host,imap_port,imap_user,imap_password,smtp_host,smtp_port,smtp_user,smtp_password,folder_map,sync_interval_minutes,initial_cutoff,retention_days,call_alert_enabled,alert_folders,display_order FROM accounts ORDER BY user_id, display_order, canonical_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Account
	for rows.Next() {
		var a Account
		var alerts int
		if err := rows.Scan(&a.ID, &a.UserID, &a.CanonicalName, &a.Email, &a.SenderName, &a.IMAPHost, &a.IMAPPort, &a.IMAPUser, &a.IMAPPassword, &a.SMTPHost, &a.SMTPPort, &a.SMTPUser, &a.SMTPPassword, &a.FolderMap, &a.SyncIntervalMinutes, &a.InitialCutoff, &a.RetentionDays, &alerts, &a.AlertFolders, &a.DisplayOrder); err != nil {
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
