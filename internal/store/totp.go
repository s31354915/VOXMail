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
	for _, hash := range codeHashes {
		if strings.TrimSpace(hash) == "" {
			return fmt.Errorf("TOTP backup-code hash is empty")
		}
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

// ConsumeBackupCode verifies and consumes one normalized or legacy backup
// code in a single transaction. The verifier is supplied by the auth layer so
// the store owns persistence while bcrypt policy stays outside it.
func (s *Store) ConsumeBackupCode(ctx context.Context, userID, candidate string, verify func(hash, candidate string) bool, at time.Time) (bool, error) {
	if userID == "" || strings.TrimSpace(candidate) == "" || verify == nil {
		return false, nil
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id,code_hash FROM totp_backup_codes WHERE user_id=? AND used_at IS NULL ORDER BY id`, userID)
	if err != nil {
		return false, err
	}
	var matchedID int64
	for rows.Next() {
		var id int64
		var hash string
		if err := rows.Scan(&id, &hash); err != nil {
			_ = rows.Close()
			return false, err
		}
		if verify(hash, candidate) {
			matchedID = id
			break
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return false, err
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	if matchedID != 0 {
		result, err := tx.ExecContext(ctx, `UPDATE totp_backup_codes SET used_at=? WHERE id=? AND user_id=? AND used_at IS NULL`, at.UTC().Format(time.RFC3339Nano), matchedID, userID)
		if err != nil {
			return false, err
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return false, nil
		}
	} else {
		// Migrate the pre-normalized JSON backup-code representation on first
		// successful use, preserving all remaining hashes in the new table.
		var raw string
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(backup_codes,'') FROM users WHERE id=?`, userID).Scan(&raw); err != nil {
			return false, err
		}
		var hashes []string
		if json.Unmarshal([]byte(raw), &hashes) != nil {
			return false, nil
		}
		legacyIndex := -1
		for i, hash := range hashes {
			if verify(hash, candidate) {
				legacyIndex = i
				break
			}
		}
		if legacyIndex < 0 {
			return false, nil
		}
		hashes = append(hashes[:legacyIndex], hashes[legacyIndex+1:]...)
		stamp := at.UTC().Format(time.RFC3339Nano)
		for _, hash := range hashes {
			if _, err := tx.ExecContext(ctx, `INSERT INTO totp_backup_codes(user_id,code_hash,created_at) VALUES(?,?,?)`, userID, hash, stamp); err != nil {
				return false, err
			}
		}
		updated, err := json.Marshal(hashes)
		if err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET backup_codes=? WHERE id=?`, string(updated), userID); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
