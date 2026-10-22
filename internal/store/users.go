package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// SetupCompleted reports the durable one-time bootstrap marker. The setup
// handler uses it as a cheap preflight before expensive credential hashing;
// CreateBootstrapUser remains the authoritative transactional guard.
func (s *Store) SetupCompleted(ctx context.Context) (bool, error) {
	var completed int
	err := s.DB.QueryRowContext(ctx, `SELECT setup_completed FROM system_settings WHERE id=1`).Scan(&completed)
	return completed != 0, err
}

// ErrUsernameTaken reports a username that another account already uses.
var ErrUsernameTaken = errors.New("username already exists")

// UpdateUsername renames one user's sign-in username. The new name must be
// unique across accounts, the same rule the setup and admin-create flows
// enforce; the unique index remains the race-free backstop.
func (s *Store) UpdateUsername(ctx context.Context, userID, username string) error {
	username = strings.TrimSpace(username)
	var existingID string
	err := s.DB.QueryRowContext(ctx, `SELECT id FROM users WHERE username=?`, username).Scan(&existingID)
	switch {
	case err == nil && existingID != userID:
		return ErrUsernameTaken
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return err
	}
	_, err = s.DB.ExecContext(ctx, `UPDATE users SET username=? WHERE id=?`, username, userID)
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed: users.username") {
		return ErrUsernameTaken
	}
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

// CreateBootstrapUser atomically claims the one-time setup slot and creates
// the first administrator plus its settings row. The durable claim prevents
// an empty users table from reopening unauthenticated setup after repair.
func (s *Store) CreateBootstrapUser(ctx context.Context, user User) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE system_settings SET setup_completed=1 WHERE id=1 AND setup_completed=0`)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrSetupCompleted
	}
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

