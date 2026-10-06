package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

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

// AlertNextDial is the restart-safe per-user alert cooldown deadline.
func (s *Store) AlertNextDial(ctx context.Context, userID string) (time.Time, error) {
	var raw string
	err := s.DB.QueryRowContext(ctx, `SELECT COALESCE(alert_next_dial,'') FROM settings WHERE user_id=?`, userID).Scan(&raw)
	if err != nil {
		return time.Time{}, err
	}
	if strings.TrimSpace(raw) == "" {
		return time.Time{}, nil
	}
	deadline, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid alert cooldown: %w", err)
	}
	return deadline, nil
}

func (s *Store) SetAlertNextDial(ctx context.Context, userID string, deadline time.Time) error {
	if strings.TrimSpace(userID) == "" {
		return fmt.Errorf("user identity is required")
	}
	value := ""
	if !deadline.IsZero() {
		value = deadline.UTC().Format(time.RFC3339Nano)
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE settings SET alert_next_dial=? WHERE user_id=?`, value, userID)
	return err
}
