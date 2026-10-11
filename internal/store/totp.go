package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// EnableTOTP replaces the user's TOTP secret and backup-code set as one
// transaction, clearing replay state from any previous authenticator.
func (s *Store) EnableTOTP(ctx context.Context, userID, sealedSecret string, codeHashes []string, createdAt time.Time) error {
	if userID == "" || sealedSecret == "" {
		return fmt.Errorf("user and TOTP secret are required")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE users SET totp_secret=?,backup_codes='' WHERE id=?`, sealedSecret, userID)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return fmt.Errorf("user not found")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM totp_backup_codes WHERE user_id=?`, userID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM totp_replays WHERE user_id=?`, userID); err != nil {
		return err
	}
	stamp := createdAt.UTC().Format(time.RFC3339Nano)
	seen := make(map[string]bool)
	for _, hash := range codeHashes {
		if strings.TrimSpace(hash) == "" {
			return fmt.Errorf("TOTP backup-code hash is empty")
		}
		if seen[hash] {
			continue
		}
		seen[hash] = true
		if _, err := tx.ExecContext(ctx, `INSERT INTO totp_backup_codes(user_id,code_hash,created_at) VALUES(?,?,?)`, userID, hash, stamp); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DisableTOTP removes the user's authenticator, backup codes, and replay
// cursor atomically.
func (s *Store) DisableTOTP(ctx context.Context, userID string) error {
	if userID == "" {
		return fmt.Errorf("user is required")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE users SET totp_secret='',backup_codes='' WHERE id=?`, userID)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return fmt.Errorf("user not found")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM totp_backup_codes WHERE user_id=?`, userID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM totp_replays WHERE user_id=?`, userID); err != nil {
		return err
	}
	return tx.Commit()
}

// ConsumeTOTPReplay accepts a clock step only when it is strictly newer than
// the user's last accepted step. The update is atomic for concurrent logins.
func (s *Store) ConsumeTOTPReplay(ctx context.Context, userID string, step int64) (bool, error) {
	if userID == "" {
		return false, fmt.Errorf("user is required")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO totp_replays(user_id,last_step) VALUES(?,?) ON CONFLICT(user_id) DO UPDATE SET last_step=excluded.last_step WHERE excluded.last_step > totp_replays.last_step`, userID, step)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return affected == 1, nil
}

// ConsumeBackupCode snapshots candidate hashes and releases the database
// connection before expensive verification. A conditional update consumes
// the exact matched row once, even if another caller or TOTP reset intervenes.
func (s *Store) ConsumeBackupCode(ctx context.Context, userID, candidate string, verify func(hash, candidate string) bool, at time.Time) (bool, error) {
	if userID == "" || strings.TrimSpace(candidate) == "" || verify == nil {
		return false, nil
	}
	if err := s.normalizeLegacyBackupCodes(ctx, userID, at); err != nil {
		return false, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT id,code_hash FROM totp_backup_codes WHERE user_id=? AND used_at IS NULL ORDER BY id`, userID)
	if err != nil {
		return false, err
	}
	type backupCandidate struct {
		id   int64
		hash string
	}
	var hashes []backupCandidate
	for rows.Next() {
		var item backupCandidate
		if err := rows.Scan(&item.id, &item.hash); err != nil {
			_ = rows.Close()
			return false, err
		}
		hashes = append(hashes, item)
	}
	scanErr := rows.Err()
	closeErr := rows.Close()
	if scanErr != nil {
		return false, scanErr
	}
	if closeErr != nil {
		return false, closeErr
	}
	for _, item := range hashes {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if !verify(item.hash, candidate) {
			continue
		}
		// IDs can be reused after EnableTOTP replaces the backup-code set. Check
		// the hash as well so verification of an old ID cannot consume a new code.
		result, err := s.DB.ExecContext(ctx, `UPDATE totp_backup_codes SET used_at=? WHERE id=? AND user_id=? AND code_hash=? AND used_at IS NULL`, at.UTC().Format(time.RFC3339Nano), item.id, userID, item.hash)
		if err != nil {
			return false, err
		}
		affected, err := result.RowsAffected()
		return affected == 1, err
	}
	return false, ctx.Err()
}

// Normalize the legacy JSON in a short transaction without hashing. Clearing
// the old representation is essential: keeping both permits a used normalized
// code to be imported again from JSON. Existing hashes (including used rows)
// also prevent reimport of copies left by earlier versions.
func (s *Store) normalizeLegacyBackupCodes(ctx context.Context, userID string, at time.Time) error {
	var raw string
	if err := s.DB.QueryRowContext(ctx, `SELECT COALESCE(backup_codes,'') FROM users WHERE id=?`, userID).Scan(&raw); err != nil {
		return err
	}
	if raw == "" {
		return nil
	}
	var hashes []string
	if json.Unmarshal([]byte(raw), &hashes) != nil {
		return nil
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE users SET backup_codes='' WHERE id=? AND backup_codes=?`, userID, raw)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return nil
	}
	stamp := at.UTC().Format(time.RFC3339Nano)
	// Earlier migrations could insert the remaining JSON hashes more than
	// once. Keep one row per hash, preferring a consumed row so duplicates
	// cannot resurrect a code that was already used.
	if _, err := tx.ExecContext(ctx, `DELETE FROM totp_backup_codes WHERE user_id=? AND id NOT IN (
		SELECT COALESCE(MIN(CASE WHEN used_at IS NOT NULL THEN id END),MIN(id))
		FROM totp_backup_codes WHERE user_id=? GROUP BY code_hash)`, userID, userID); err != nil {
		return err
	}
	for _, hash := range hashes {
		if strings.TrimSpace(hash) == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO totp_backup_codes(user_id,code_hash,created_at) SELECT ?,?,? WHERE NOT EXISTS (SELECT 1 FROM totp_backup_codes WHERE user_id=? AND code_hash=?)`, userID, hash, stamp, userID, hash); err != nil {
			return err
		}
	}
	return tx.Commit()
}
