package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrRecoveryReservationSuperseded = errors.New("recovery reservation was superseded")

type RecoveryCandidate struct {
	ID        int64
	UserID    string
	TokenHash string
	ExpiresAt string
}

// ReserveRecoveryToken creates a pending, owner-scoped recovery token after
// enforcing the durable per-user issuance limit. Existing tokens for the same
// purpose are invalidated in the same transaction so only the newest message
// can become usable.
func (s *Store) ReserveRecoveryToken(ctx context.Context, userID, email, purpose string, expiresAt, createdAt time.Time, window time.Duration, limit int) (int64, string, bool, error) {
	if userID == "" || strings.TrimSpace(email) == "" || strings.TrimSpace(purpose) == "" {
		return 0, "", false, fmt.Errorf("recovery identity and purpose are required")
	}
	if limit <= 0 || window <= 0 {
		return 0, "", false, fmt.Errorf("recovery limit and window must be positive")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, "", false, err
	}
	defer tx.Rollback()
	cutoff := createdAt.Add(-window).Format(time.RFC3339Nano)
	var recent int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM recovery_tokens WHERE user_id=? AND created_at >= ?`, userID, cutoff).Scan(&recent); err != nil {
		return 0, "", false, err
	}
	if recent >= limit {
		if err := tx.Commit(); err != nil {
			return 0, "", false, err
		}
		return 0, "", false, nil
	}
	placeholder, err := recoveryPlaceholder()
	if err != nil {
		return 0, "", false, err
	}
	now := createdAt.UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `UPDATE recovery_tokens SET used_at=? WHERE user_id=? AND purpose=? AND used_at IS NULL`, now, userID, purpose); err != nil {
		return 0, "", false, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO recovery_tokens(user_id,email,token_hash,purpose,expires_at,created_at) VALUES(?,?,?,?,?,?)`, userID, strings.ToLower(email), placeholder, purpose, expiresAt.UTC().Format(time.RFC3339Nano), now)
	if err != nil {
		return 0, "", false, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, "", false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, "", false, err
	}
	return id, placeholder, true, nil
}

func recoveryPlaceholder() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "pending:" + hex.EncodeToString(raw[:]), nil
}

func (s *Store) SetRecoveryTokenHash(ctx context.Context, id int64, placeholder, hash string) error {
	if id <= 0 || placeholder == "" || hash == "" {
		return fmt.Errorf("recovery token identity and hash are required")
	}
	result, err := s.DB.ExecContext(ctx, `UPDATE recovery_tokens SET token_hash=? WHERE id=? AND token_hash=? AND used_at IS NULL`, hash, id, placeholder)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrRecoveryReservationSuperseded
	}
	return nil
}

func (s *Store) InvalidateRecoveryToken(ctx context.Context, id int64, at time.Time) error {
	if id <= 0 {
		return fmt.Errorf("recovery token identity is required")
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE recovery_tokens SET used_at=? WHERE id=? AND used_at IS NULL`, at.UTC().Format(time.RFC3339Nano), id)
	return err
}

// TakeRecoveryCandidate atomically claims one eligible candidate for
// verification by incrementing its bounded attempt counter. Hash comparison
// stays outside the store because it belongs to the authentication layer.
func (s *Store) TakeRecoveryCandidate(ctx context.Context, email, purpose string, skipped map[int64]struct{}, now time.Time, maxCandidates int) (RecoveryCandidate, bool, error) {
	if strings.TrimSpace(email) == "" || strings.TrimSpace(purpose) == "" || maxCandidates <= 0 {
		return RecoveryCandidate{}, false, fmt.Errorf("recovery lookup is incomplete")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return RecoveryCandidate{}, false, err
	}
	defer tx.Rollback()
	query := fmt.Sprintf(`SELECT id,user_id,token_hash,expires_at,attempts FROM recovery_tokens WHERE lower(email)=lower(?) AND purpose=? AND used_at IS NULL AND token_hash NOT LIKE 'pending:%%' ORDER BY id DESC LIMIT %d`, maxCandidates)
	rows, err := tx.QueryContext(ctx, query, email, purpose)
	if err != nil {
		return RecoveryCandidate{}, false, err
	}
	var candidates []struct {
		candidate RecoveryCandidate
		attempts  int64
	}
	for rows.Next() {
		var item struct {
			candidate RecoveryCandidate
			attempts  int64
		}
		if err := rows.Scan(&item.candidate.ID, &item.candidate.UserID, &item.candidate.TokenHash, &item.candidate.ExpiresAt, &item.attempts); err != nil {
			_ = rows.Close()
			return RecoveryCandidate{}, false, err
		}
		candidates = append(candidates, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return RecoveryCandidate{}, false, err
	}
	if err := rows.Close(); err != nil {
		return RecoveryCandidate{}, false, err
	}
	current := now.UTC()
	var selected RecoveryCandidate
	for _, item := range candidates {
		if _, alreadyTried := skipped[item.candidate.ID]; alreadyTried || item.attempts >= 5 {
			continue
		}
		expires, parseErr := time.Parse(time.RFC3339Nano, item.candidate.ExpiresAt)
		if parseErr != nil || current.After(expires) {
			if _, err := tx.ExecContext(ctx, `UPDATE recovery_tokens SET used_at=? WHERE id=? AND used_at IS NULL`, current.Format(time.RFC3339Nano), item.candidate.ID); err != nil {
				return RecoveryCandidate{}, false, err
			}
			continue
		}
		result, err := tx.ExecContext(ctx, `UPDATE recovery_tokens SET attempts=attempts+1 WHERE id=? AND used_at IS NULL AND attempts < 5`, item.candidate.ID)
		if err != nil {
			return RecoveryCandidate{}, false, err
		}
		if affected, _ := result.RowsAffected(); affected == 1 {
			selected = item.candidate
			break
		}
	}
	if err := tx.Commit(); err != nil {
		return RecoveryCandidate{}, false, err
	}
	return selected, selected.ID != 0, nil
}

// RedeemRecoveryResult distinguishes a stale token from a valid token tied to
// a disabled user without exposing that distinction to the HTTP response.
type RedeemRecoveryResult struct {
	Redeemed     bool
	DisabledUser bool
}

// RedeemRecoveryToken consumes the token and applies an optional password
// hash in one transaction.
func (s *Store) RedeemRecoveryToken(ctx context.Context, tokenID int64, userID, passwordHash string, at time.Time) (RedeemRecoveryResult, error) {
	if tokenID <= 0 || userID == "" {
		return RedeemRecoveryResult{}, fmt.Errorf("recovery token and user are required")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return RedeemRecoveryResult{}, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE recovery_tokens SET used_at=? WHERE id=? AND user_id=? AND used_at IS NULL`, at.UTC().Format(time.RFC3339Nano), tokenID, userID)
	if err != nil {
		return RedeemRecoveryResult{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return RedeemRecoveryResult{}, nil
	}
	var enabled int
	if err := tx.QueryRowContext(ctx, `SELECT enabled FROM users WHERE id=?`, userID).Scan(&enabled); err != nil {
		return RedeemRecoveryResult{}, err
	}
	if enabled == 0 {
		if err := tx.Commit(); err != nil {
			return RedeemRecoveryResult{}, err
		}
		return RedeemRecoveryResult{DisabledUser: true}, nil
	}
	if passwordHash != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE users SET password_hash=? WHERE id=?`, passwordHash, userID); err != nil {
			return RedeemRecoveryResult{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return RedeemRecoveryResult{}, err
	}
	return RedeemRecoveryResult{Redeemed: true}, nil
}
