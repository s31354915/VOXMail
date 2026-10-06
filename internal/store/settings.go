package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// SpeechSettings is the typed, call-layer view of the user speech settings.
// Keeping the SQL projection here prevents IVR code from depending on the
// settings table layout.
type SpeechSettings struct {
	Voice      string
	MenuSpeed  int
	EmailSpeed int
}

// UserSettings is the typed representation of one user's settings row.
// AlertPhone remains a pointer so a deliberately absent destination is not
// confused with a configured empty value at the API boundary.
type UserSettings struct {
	TTSVoice      string
	MenuSpeed     int
	EmailSpeed    int
	AlertsEnabled bool
	AlertPhone    *string
}

// UserSettings returns all user-scoped preferences needed by HTTP, speech,
// and call-alert workflows.
func (s *Store) UserSettings(ctx context.Context, userID string) (UserSettings, error) {
	if userID == "" {
		return UserSettings{}, fmt.Errorf("user is required")
	}
	var settings UserSettings
	err := s.DB.QueryRowContext(ctx, `SELECT tts_voice,menu_speed,email_speed,alerts_enabled,alert_phone FROM settings WHERE user_id=?`, userID).
		Scan(&settings.TTSVoice, &settings.MenuSpeed, &settings.EmailSpeed, &settings.AlertsEnabled, &settings.AlertPhone)
	return settings, err
}

// SaveUserSettings atomically upserts the complete user preference row.
func (s *Store) SaveUserSettings(ctx context.Context, userID string, settings UserSettings) error {
	if userID == "" {
		return fmt.Errorf("user is required")
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO settings(user_id,tts_voice,menu_speed,email_speed,alerts_enabled,alert_phone) VALUES(?,?,?,?,?,?) ON CONFLICT(user_id) DO UPDATE SET tts_voice=excluded.tts_voice,menu_speed=excluded.menu_speed,email_speed=excluded.email_speed,alerts_enabled=excluded.alerts_enabled,alert_phone=excluded.alert_phone`, userID, settings.TTSVoice, settings.MenuSpeed, settings.EmailSpeed, settings.AlertsEnabled, settings.AlertPhone)
	return err
}

// SpeechSettings returns the persisted speech settings for one user.
func (s *Store) SpeechSettings(ctx context.Context, userID string) (SpeechSettings, error) {
	settings, err := s.UserSettings(ctx, userID)
	return SpeechSettings{Voice: settings.TTSVoice, MenuSpeed: settings.MenuSpeed, EmailSpeed: settings.EmailSpeed}, err
}

// SetAccountCallAlertEnabled updates an account only when it belongs to the
// supplied user. A missing or foreign account is reported as sql.ErrNoRows.
func (s *Store) SetAccountCallAlertEnabled(ctx context.Context, userID, accountID string, enabled bool) error {
	if userID == "" || accountID == "" {
		return fmt.Errorf("user and account are required")
	}
	result, err := s.DB.ExecContext(ctx, `UPDATE accounts SET call_alert_enabled=? WHERE id=? AND user_id=?`, enabled, accountID, userID)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected != 1 {
		return sql.ErrNoRows
	}
	return nil
}

// SetAccountAlertFolders persists the selected local folder names for an
// owner-scoped account. JSON encoding is part of the store contract rather
// than a concern of the IVR state machine.
func (s *Store) SetAccountAlertFolders(ctx context.Context, userID, accountID string, folders []string) error {
	if userID == "" || accountID == "" {
		return fmt.Errorf("user and account are required")
	}
	encoded, err := json.Marshal(folders)
	if err != nil {
		return err
	}
	result, err := s.DB.ExecContext(ctx, `UPDATE accounts SET alert_folders=? WHERE id=? AND user_id=?`, string(encoded), accountID, userID)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected != 1 {
		return sql.ErrNoRows
	}
	return nil
}

// AlertsEnabled returns the user's global call-alert preference.
func (s *Store) AlertsEnabled(ctx context.Context, userID string) (bool, error) {
	if userID == "" {
		return false, fmt.Errorf("user is required")
	}
	var enabled bool
	err := s.DB.QueryRowContext(ctx, `SELECT alerts_enabled FROM settings WHERE user_id=?`, userID).Scan(&enabled)
	return enabled, err
}

// SetAlertsEnabled changes only the owner's global call-alert preference.
func (s *Store) SetAlertsEnabled(ctx context.Context, userID string, enabled bool) error {
	if userID == "" {
		return fmt.Errorf("user is required")
	}
	result, err := s.DB.ExecContext(ctx, `UPDATE settings SET alerts_enabled=? WHERE user_id=?`, enabled, userID)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected != 1 {
		return sql.ErrNoRows
	}
	return nil
}

// UpdatePasswordHash replaces only the owner-scoped password credential.
func (s *Store) UpdatePasswordHash(ctx context.Context, userID, hash string) error {
	return s.updateCredentialHash(ctx, `UPDATE users SET password_hash=? WHERE id=?`, hash, userID)
}

// UpdatePINHash replaces only the owner-scoped PIN credential.
func (s *Store) UpdatePINHash(ctx context.Context, userID, hash string) error {
	return s.updateCredentialHash(ctx, `UPDATE users SET pin_hash=? WHERE id=?`, hash, userID)
}

func (s *Store) updateCredentialHash(ctx context.Context, query, hash, userID string) error {
	if userID == "" || hash == "" {
		return fmt.Errorf("user and credential hash are required")
	}
	result, err := s.DB.ExecContext(ctx, query, hash, userID)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected != 1 {
		return sql.ErrNoRows
	}
	return nil
}
