package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

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
