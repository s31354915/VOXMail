package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// CreateVoiceInstallJob persists acceptance of an administrator voice job
// before any asynchronous work is started.
func (s *Store) CreateVoiceInstallJob(ctx context.Context, job VoiceInstallJob) error {
	if job.ID == "" || job.UserID == "" || job.Voice == "" {
		return fmt.Errorf("voice job identity is required")
	}
	if job.Stage == "" {
		job.Stage = "queued"
	}
	if job.Status == "" {
		job.Status = "queued"
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if job.CreatedAt == "" {
		job.CreatedAt = now
	}
	if job.UpdatedAt == "" {
		job.UpdatedAt = job.CreatedAt
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO voice_install_jobs(id,user_id,voice,stage,status,progress,error,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		job.ID, job.UserID, job.Voice, job.Stage, job.Status, job.Progress, job.Error, job.CreatedAt, job.UpdatedAt)
	return err
}

// UpdateVoiceInstallJob changes only the status fields of an existing job.
// The affected-row check prevents a late worker update from recreating a job
// that has already been removed by retention or user deletion.
func (s *Store) UpdateVoiceInstallJob(ctx context.Context, job VoiceInstallJob) error {
	if job.ID == "" {
		return fmt.Errorf("voice job ID is required")
	}
	if job.UpdatedAt == "" {
		job.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	result, err := s.DB.ExecContext(ctx, `UPDATE voice_install_jobs SET stage=?,status=?,progress=?,error=?,updated_at=? WHERE id=?`,
		job.Stage, job.Status, job.Progress, job.Error, job.UpdatedAt, job.ID)
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

func (s *Store) VoiceInstallJob(ctx context.Context, id string) (VoiceInstallJob, error) {
	var job VoiceInstallJob
	err := s.DB.QueryRowContext(ctx, `SELECT id,user_id,voice,stage,status,progress,error,created_at,updated_at FROM voice_install_jobs WHERE id=?`, id).
		Scan(&job.ID, &job.UserID, &job.Voice, &job.Stage, &job.Status, &job.Progress, &job.Error, &job.CreatedAt, &job.UpdatedAt)
	return job, err
}

// MarkInterruptedVoiceInstallJobs converts work that was in flight when the
// process died into an explicit terminal state. New workers must be started by
// a fresh request; silently resuming a partially downloaded/activated model
// would make the job status lie about what was actually completed.
func (s *Store) MarkInterruptedVoiceInstallJobs(ctx context.Context, now time.Time) error {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE voice_install_jobs SET stage='failed',status='failed',progress=100,error='voice installation interrupted by server restart',updated_at=? WHERE status IN ('queued','running')`, now.UTC().Format(time.RFC3339Nano))
	return err
}

// PurgeVoiceInstallJobs removes only terminal job history older than cutoff.
// Queued/running rows are never deleted by retention because they are needed
// for restart recovery and diagnosis.
func (s *Store) PurgeVoiceInstallJobs(ctx context.Context, cutoff time.Time) error {
	if cutoff.IsZero() {
		return fmt.Errorf("voice job cutoff is required")
	}
	_, err := s.DB.ExecContext(ctx, `DELETE FROM voice_install_jobs WHERE status IN ('complete','failed') AND updated_at < ?`, cutoff.UTC().Format(time.RFC3339Nano))
	return err
}
