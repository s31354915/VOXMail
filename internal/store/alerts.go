package store

import (
	"context"
	"strings"
	"time"
)

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
	rows, err := s.DB.QueryContext(ctx, `WITH eligible AS (
SELECT m.id AS id,m.account_id AS account_id,m.folder AS folder,COALESCE(m.sender,'') AS sender,COALESCE(m.subject,'') AS subject,u.id AS user_id,COALESCE(n.number,s.alert_phone,'') AS phone,s.tts_voice AS voice,COALESCE(a.alert_folders,'[]') AS alert_folders,COALESCE(m.message_date,'') AS date,m.message_date_utc AS message_date_utc,
       ROW_NUMBER() OVER (PARTITION BY u.id ORDER BY CASE WHEN m.message_date_utc IS NULL THEN 1 ELSE 0 END, m.message_date_utc DESC, m.id DESC) AS user_rank
FROM mail_messages m
JOIN accounts a ON a.id = m.account_id
JOIN users u ON u.id = a.user_id
JOIN settings s ON s.user_id = u.id
LEFT JOIN alert_numbers n ON n.user_id=u.id AND n.active=1
JOIN json_each(CASE WHEN json_valid(COALESCE(a.alert_folders,'[]')) THEN a.alert_folders ELSE '[]' END) af
  ON lower(trim(CAST(af.value AS TEXT))) = lower(trim(m.folder))
WHERE m.is_read = 0 AND m.alerted = 0
  AND a.call_alert_enabled = 1 AND s.alerts_enabled = 1 AND u.enabled = 1
  AND COALESCE(n.number,s.alert_phone,'') <> ''
  AND NOT EXISTS (SELECT 1 FROM alert_claims c WHERE c.message_id=m.id AND c.announced_at IS NULL)
)
SELECT id,account_id,folder,sender,subject,user_id,phone,voice,alert_folders,date
FROM eligible
WHERE user_rank <= 10
ORDER BY CASE WHEN message_date_utc IS NULL THEN 1 ELSE 0 END, message_date_utc DESC, id DESC
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
SELECT m.id,a.user_id,? FROM mail_messages m
JOIN accounts a ON a.id=m.account_id
JOIN users u ON u.id=a.user_id
JOIN settings s ON s.user_id=u.id
LEFT JOIN alert_numbers n ON n.user_id=u.id AND n.active=1
JOIN json_each(CASE WHEN json_valid(COALESCE(a.alert_folders,'[]')) THEN a.alert_folders ELSE '[]' END) af
  ON lower(trim(CAST(af.value AS TEXT))) = lower(trim(m.folder))
WHERE m.id=? AND a.user_id=? AND m.is_read=0 AND m.alerted=0
  AND a.call_alert_enabled=1 AND s.alerts_enabled=1 AND u.enabled=1
  AND COALESCE(n.number,s.alert_phone,'') <> ''
  AND COALESCE((SELECT alerts_available FROM system_settings WHERE id=1),0)=1`, claimedAt, id, userID)
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
