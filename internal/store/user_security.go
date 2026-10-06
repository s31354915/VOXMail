package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

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
