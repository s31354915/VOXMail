package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

type CleanupStatus struct {
	ID        string `json:"id"`
	UserID    string `json:"user_id"`
	AccountID string `json:"account_id"`
	Status    string `json:"status"`
	Attempts  int    `json:"attempts"`
	UpdatedAt string `json:"updated_at"`
	Ready     bool   `json:"ready"`
}

type CleanupSummary struct {
	Pending int `json:"pending"`
	Failed  int `json:"failed"`
}

func (s *Store) CleanupSummary(ctx context.Context, userID string, admin bool) (CleanupSummary, error) {
	var result CleanupSummary
	err := s.DB.QueryRowContext(ctx, `SELECT COALESCE(SUM(status='pending'),0),COALESCE(SUM(status='failed'),0)
FROM cleanup_jobs WHERE user_id=? OR ?`, userID, admin).Scan(&result.Pending, &result.Failed)
	return result, err
}

// ListCleanupStatuses omits private paths and raw filesystem errors. Owners
// see their own jobs; administrators can recover jobs after deleting a user.
func (s *Store) ListCleanupStatuses(ctx context.Context, userID string, admin bool) ([]CleanupStatus, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT j.id,j.user_id,j.account_id,j.status,j.attempts,j.updated_at,
NOT EXISTS (SELECT 1 FROM accounts a WHERE a.id=j.account_id)
FROM cleanup_jobs j WHERE (j.user_id=? OR ?) ORDER BY j.updated_at DESC,j.id DESC LIMIT 100`, userID, admin)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]CleanupStatus, 0)
	for rows.Next() {
		var j CleanupStatus
		if err := rows.Scan(&j.ID, &j.UserID, &j.AccountID, &j.Status, &j.Attempts, &j.UpdatedAt, &j.Ready); err != nil {
			return nil, err
		}
		result = append(result, j)
	}
	return result, rows.Err()
}

func (s *Store) RetryCleanupJob(ctx context.Context, userID, jobID string, admin bool) error {
	result, err := s.DB.ExecContext(ctx, `UPDATE cleanup_jobs SET status='pending',last_error='',updated_at=?
WHERE id=? AND (user_id=? OR ?) AND status='failed'
AND NOT EXISTS (SELECT 1 FROM accounts a WHERE a.id=cleanup_jobs.account_id)`, time.Now().UTC().Format(time.RFC3339Nano), jobID, userID, admin)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// AccountIDsForDeletion includes interrupted deletions hidden from ordinary
// navigation, ensuring user deletion still cancels every account's workers.
func (s *Store) AccountIDsForDeletion(ctx context.Context, userID string) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id FROM accounts WHERE user_id=? ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ReadyCleanupJobs never returns a job while its account still exists: sync
// cancellation and call invalidation must finish before metadata is removed.
// Failed jobs retry at a slow cadence; canceled passes remain pending and can
// resume immediately after restart. The single application worker serializes
// execution and reads only a bounded batch into memory.
func (s *Store) ReadyCleanupJobs(ctx context.Context) ([]CleanupJob, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT j.id,j.user_id,j.account_id,j.paths FROM cleanup_jobs j
WHERE NOT EXISTS (SELECT 1 FROM accounts a WHERE a.id=j.account_id)
AND (j.status='pending' OR (j.status='failed' AND j.updated_at<?))
ORDER BY j.updated_at,j.id LIMIT 8`, time.Now().UTC().Add(-5*time.Minute).Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []CleanupJob
	for rows.Next() {
		var job CleanupJob
		var paths string
		if err := rows.Scan(&job.ID, &job.UserID, &job.AccountID, &paths); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(paths), &job.Paths); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}
