package store

import (
	"context"
	"fmt"
	"time"
)

// VoiceKeepWarm records which installed Piper voices the administrator has
// pinned as always-warm. Rows survive restarts so pinned models reload on
// startup without a call arriving first.
func (s *Store) KeepWarmVoices(ctx context.Context) (map[string]bool, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT voice, keep_warm FROM voice_keep_warm`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]bool)
	for rows.Next() {
		var voice string
		var enabled int
		if err := rows.Scan(&voice, &enabled); err != nil {
			return nil, err
		}
		result[voice] = enabled != 0
	}
	return result, rows.Err()
}

// SetVoiceKeepWarm pins or unpins one installed voice.
func (s *Store) SetVoiceKeepWarm(ctx context.Context, voice string, enabled bool) error {
	if voice == "" {
		return fmt.Errorf("voice is required")
	}
	value := 0
	if enabled {
		value = 1
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO voice_keep_warm(voice,keep_warm,updated_at) VALUES(?,?,?) ON CONFLICT(voice) DO UPDATE SET keep_warm=excluded.keep_warm, updated_at=excluded.updated_at`,
		voice, value, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}
