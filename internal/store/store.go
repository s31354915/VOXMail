package store

import (
	"context"
	"database/sql"
	"fmt"
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
CREATE TABLE IF NOT EXISTS audit_log (
 id INTEGER PRIMARY KEY, user_id TEXT, action TEXT NOT NULL, detail TEXT,
 created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS mail_messages (
 id INTEGER PRIMARY KEY, account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 folder TEXT NOT NULL, path TEXT NOT NULL UNIQUE, message_id TEXT, sender TEXT,
 recipients TEXT, subject TEXT, message_date TEXT, is_read INTEGER NOT NULL DEFAULT 0,
 attachment_count INTEGER NOT NULL DEFAULT 0, updated_at TEXT NOT NULL
);`)
	if err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	return nil
}

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
	ID, Username, PasswordHash, PINHash, Role string
	Enabled                                   bool
	TOTPSecret, BackupCodes                   string
}

type Account struct {
	ID, UserID, CanonicalName, Email, SenderName string
	IMAPHost, IMAPUser, SMTPHost, SMTPUser       string
	IMAPPort, SMTPPort                           int
	IMAPPassword, SMTPPassword                   string
	FolderMap, AlertFolders                      string
	SyncIntervalMinutes, DisplayOrder            int
	InitialCutoff                                *string
	RetentionDays                                *int
	CallAlertEnabled                             bool
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

func (s *Store) MarkMailRead(ctx context.Context, userID string, id int64, read bool) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE mail_messages SET is_read=? WHERE id=? AND account_id IN (SELECT id FROM accounts WHERE user_id=?)`, read, id, userID)
	return err
}

func (s *Store) DeleteMailIndex(ctx context.Context, userID string, id int64) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM mail_messages WHERE id=? AND account_id IN (SELECT id FROM accounts WHERE user_id=?)`, id, userID)
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

func (s *Store) AddContact(ctx context.Context, contact Contact) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO contacts(user_id,name,email,display_order) VALUES (?,?,?,?)`, contact.UserID, contact.Name, contact.Email, contact.DisplayOrder)
	return err
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
