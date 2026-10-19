package store

import (
	"context"
	"github.com/voxmail/voxmail/internal/operation"
	"strings"
	"time"
)

// InterruptSyncRuns is called once by the application before admitting work.
// Opening a Store alone must not finalize another process's active runs.
func (s *Store) InterruptSyncRuns(ctx context.Context) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `UPDATE sync_runs SET finished_at=?,success=0,error='interrupted by application restart',error_category='interrupted' WHERE finished_at IS NULL OR finished_at=''`, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// BeginSyncRun records a sync before any external work starts. This makes a
// running or interrupted sync visible after a process restart.
func (s *Store) BeginSyncRun(ctx context.Context, accountID, kind string) (int64, error) {
	if strings.TrimSpace(kind) == "" {
		kind = "incremental"
	}
	res, err := s.DB.ExecContext(ctx, `INSERT INTO sync_runs(account_id,kind,started_at,success,changed,error) VALUES(?,?,?,0,0,'')`, accountID, kind, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) FinishSyncRun(ctx context.Context, id int64, success, changed bool, runErr error) error {
	if id == 0 {
		return nil
	}
	errText, category := "", ""
	if runErr != nil {
		errText = runErr.Error()
		category = string(operation.Classify(runErr))
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE sync_runs SET finished_at=?,success=?,changed=?,error=?,error_category=? WHERE id=?`, time.Now().UTC().Format(time.RFC3339Nano), success, changed, errText, category, id)
	return err
}

func (s *Store) LatestSyncRun(ctx context.Context, accountID string, successfulOnly bool) (SyncRun, error) {
	query := `SELECT id,account_id,kind,started_at,COALESCE(finished_at,''),success,changed,error,error_category FROM sync_runs WHERE account_id=?`
	if successfulOnly {
		query += ` AND success=1`
	}
	query += ` ORDER BY id DESC LIMIT 1`
	var run SyncRun
	var success, changed int
	err := s.DB.QueryRowContext(ctx, query, accountID).Scan(&run.ID, &run.AccountID, &run.Kind, &run.StartedAt, &run.FinishedAt, &success, &changed, &run.Error, &run.ErrorCategory)
	if err != nil {
		return SyncRun{}, err
	}
	run.Success = success != 0
	run.Changed = changed != 0
	return run, nil
}

// LatestSyncRuns returns, for each account id, its most recent run (or most
// recent successful run when successfulOnly is set) in one query, so listing
// several accounts does not issue two queries per account.
func (s *Store) LatestSyncRuns(ctx context.Context, accountIDs []string, successfulOnly bool) (map[string]SyncRun, error) {
	out := make(map[string]SyncRun, len(accountIDs))
	if len(accountIDs) == 0 {
		return out, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(accountIDs)), ",")
	args := make([]any, 0, len(accountIDs))
	for _, id := range accountIDs {
		args = append(args, id)
	}
	query := `SELECT r.account_id,r.id,r.kind,r.started_at,COALESCE(r.finished_at,''),r.success,r.changed,r.error,r.error_category FROM sync_runs r JOIN (SELECT account_id,MAX(id) mid FROM sync_runs WHERE account_id IN (` + placeholders + `)`
	if successfulOnly {
		query += ` AND success=1`
	}
	query += ` GROUP BY account_id) m ON r.id=m.mid`
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var run SyncRun
		var success, changed int
		if err := rows.Scan(&run.AccountID, &run.ID, &run.Kind, &run.StartedAt, &run.FinishedAt, &success, &changed, &run.Error, &run.ErrorCategory); err != nil {
			return nil, err
		}
		run.Success = success != 0
		run.Changed = changed != 0
		out[run.AccountID] = run
	}
	return out, rows.Err()
}

// LatestFullSyncRun returns the most recent successful run that performs a
// complete remote identity walk. Incremental runs intentionally do not update
// this clock.
func (s *Store) LatestFullSyncRun(ctx context.Context, accountID string) (SyncRun, error) {
	var run SyncRun
	var success, changed int
	err := s.DB.QueryRowContext(ctx, `SELECT id,account_id,kind,started_at,COALESCE(finished_at,''),success,changed,error,error_category FROM sync_runs WHERE account_id=? AND success=1 AND kind IN ('initial','reconciliation','manual') ORDER BY id DESC LIMIT 1`, accountID).Scan(&run.ID, &run.AccountID, &run.Kind, &run.StartedAt, &run.FinishedAt, &success, &changed, &run.Error, &run.ErrorCategory)
	if err != nil {
		return SyncRun{}, err
	}
	run.Success = success != 0
	run.Changed = changed != 0
	return run, nil
}

func (s *Store) ListSyncRuns(ctx context.Context, userID, accountID string, limit int) ([]SyncRun, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	query := `SELECT r.id,r.account_id,r.kind,r.started_at,COALESCE(r.finished_at,''),r.success,r.changed,r.error,r.error_category FROM sync_runs r JOIN accounts a ON a.id=r.account_id WHERE a.user_id=?`
	args := []any{userID}
	if accountID != "" {
		query += ` AND r.account_id=?`
		args = append(args, accountID)
	}
	query += ` ORDER BY r.id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SyncRun
	for rows.Next() {
		var run SyncRun
		var success, changed int
		if err := rows.Scan(&run.ID, &run.AccountID, &run.Kind, &run.StartedAt, &run.FinishedAt, &success, &changed, &run.Error, &run.ErrorCategory); err != nil {
			return nil, err
		}
		run.Success = success != 0
		run.Changed = changed != 0
		out = append(out, run)
	}
	return out, rows.Err()
}
