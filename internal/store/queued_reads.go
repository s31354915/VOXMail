package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// QueuedReadMutation combines the latest pending journal row with the Maildir
// state currently indexed. New journal rows preserve the requested flag and
// identity; legacy rows fall back to their indexed Maildir state.
type QueuedReadMutation struct {
	MutationID int64
	CacheRead  bool
	MailSummary
}

func (s *Store) QueuedReadMutations(ctx context.Context, accountID string, watermark int64) ([]QueuedReadMutation, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT q.id,m.id,m.account_id,m.folder,m.path,COALESCE(m.message_id,'') AS message_id,COALESCE(q.desired_read,m.is_read),COALESCE(NULLIF(q.source_uid,0),m.imap_uid,0),COALESCE(NULLIF(q.source_uid_validity,0),m.uid_validity,0),m.is_read
 FROM message_mutations q JOIN mail_messages m ON m.id=q.message_id
 WHERE m.account_id=? AND q.operation='mark_read' AND q.status='queued' AND q.from_folder=m.folder AND q.id<=?
 AND q.id=(SELECT MAX(p.id) FROM message_mutations p WHERE p.message_id=m.id AND p.operation='mark_read' AND p.status='queued' AND p.id<=?)
 ORDER BY q.id`, accountID, watermark, watermark)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var pending []QueuedReadMutation
	for rows.Next() {
		var item QueuedReadMutation
		if err := rows.Scan(&item.MutationID, &item.ID, &item.AccountID, &item.Folder, &item.Path, &item.MessageID, &item.Read, &item.UID, &item.UIDValidity, &item.CacheRead); err != nil {
			return nil, err
		}
		pending = append(pending, item)
	}
	return pending, rows.Err()
}

// LatestReadForIdentity orders user actions across physical remaps of the same
// exact provider folder/UID/epoch. Completed actions also prevent an older
// retained queue from overwriting a newer, already synchronized choice.
func (s *Store) LatestReadForIdentity(ctx context.Context, accountID string, folders []string, uid, epoch uint32) (QueuedReadMutation, error) {
	var item QueuedReadMutation
	if len(folders) == 0 || uid == 0 || epoch == 0 {
		return item, sql.ErrNoRows
	}
	query := `SELECT q.id,m.id,m.account_id,m.folder,m.path,COALESCE(q.desired_read,m.is_read),COALESCE(NULLIF(q.source_uid,0),m.imap_uid,0),COALESCE(NULLIF(q.source_uid_validity,0),m.uid_validity,0),m.is_read
 FROM message_mutations q JOIN mail_messages m ON m.id=q.message_id
 WHERE m.account_id=? AND q.operation='mark_read' AND q.status IN ('queued','synchronized','success') AND q.from_folder=m.folder
 AND COALESCE(NULLIF(q.source_uid,0),m.imap_uid,0)=? AND COALESCE(NULLIF(q.source_uid_validity,0),m.uid_validity,0)=?
 AND COALESCE(m.imap_uid,0)=? AND COALESCE(m.uid_validity,0)=?
 AND m.folder IN (` + strings.TrimSuffix(strings.Repeat("?,", len(folders)), ",") + `) ORDER BY q.id DESC LIMIT 1`
	args := []any{accountID, uid, epoch, uid, epoch}
	for _, folder := range folders {
		args = append(args, folder)
	}
	err := s.DB.QueryRowContext(ctx, query, args...).Scan(&item.MutationID, &item.ID, &item.AccountID, &item.Folder, &item.Path, &item.Read, &item.UID, &item.UIDValidity, &item.CacheRead)
	return item, err
}

// FinishQueuedRead records provider confirmation only while the indexed source,
// stable identity and desired flag still match the attempted snapshot. A newer
// toggle or a moved message stays queued for another attempt. Failures preserve
// the queue and expose the diagnostic through the existing mutation journal.
func (s *Store) FinishQueuedRead(ctx context.Context, item QueuedReadMutation, mutationErr error) error {
	status, detail := "synchronized", ""
	if mutationErr != nil {
		status, detail = "queued", mutationErr.Error()
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE message_mutations SET status=?,error=?
 WHERE message_id=? AND id<=? AND status='queued' AND operation='mark_read' AND from_folder=?
 AND (source_uid IS NULL OR source_uid=0 OR source_uid=?) AND (source_uid_validity IS NULL OR source_uid_validity=0 OR source_uid_validity=?)
 AND EXISTS(SELECT 1 FROM mail_messages m WHERE m.id=message_mutations.message_id AND m.account_id=? AND m.folder=? AND m.is_read=? AND COALESCE(m.imap_uid,0)=? AND COALESCE(m.uid_validity,0)=?)`, status, detail, item.ID, item.MutationID, item.Folder, item.UID, item.UIDValidity, item.AccountID, item.Folder, item.Read, item.UID, item.UIDValidity)
	return err
}

// LockMailReadState serializes local flag/path settlement with user toggles.
// Provider network operations take place before acquiring this short-lived lock.
func (s *Store) LockMailReadState() func() {
	s.mailReadState.Lock()
	return s.mailReadState.Unlock
}

func (s *Store) ReadIdentityCopies(ctx context.Context, accountID string, folders []string, uid, epoch uint32) ([]MailSummary, error) {
	if len(folders) == 0 {
		return nil, nil
	}
	query := `SELECT id,account_id,folder,path,is_read,COALESCE(imap_uid,0),COALESCE(uid_validity,0) FROM mail_messages WHERE account_id=? AND imap_uid=? AND uid_validity=? AND folder IN (` + strings.TrimSuffix(strings.Repeat("?,", len(folders)), ",") + `)`
	args := []any{accountID, uid, epoch}
	for _, folder := range folders {
		args = append(args, folder)
	}
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var copies []MailSummary
	for rows.Next() {
		var item MailSummary
		if err := rows.Scan(&item.ID, &item.AccountID, &item.Folder, &item.Path, &item.Read, &item.UID, &item.UIDValidity); err != nil {
			return nil, err
		}
		copies = append(copies, item)
	}
	return copies, rows.Err()
}

// MarkConfirmedReadAtPath refuses a concurrent move, reindex or identity change.
func (s *Store) MarkConfirmedReadAtPath(ctx context.Context, item MailSummary, read bool, path string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// The indexer may have already indexed the renamed file under the target
	// path; merge that row instead of crashing on UNIQUE(path).
	if err := s.mergeMailPathCollision(ctx, tx, item.AccountID, item.ID, path); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE mail_messages SET is_read=?,path=? WHERE id=? AND account_id=? AND folder=? AND path=? AND COALESCE(imap_uid,0)=? AND COALESCE(uid_validity,0)=?`, read, path, item.ID, item.AccountID, item.Folder, item.Path, item.UID, item.UIDValidity)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

// RecordQueuedRead preserves the user's requested flag and observed identity,
// rather than recapturing a flag that concurrent indexing may have refreshed.
func (s *Store) RecordQueuedRead(ctx context.Context, userID string, item MailSummary, read bool) error {
	result, err := s.DB.ExecContext(ctx, `INSERT INTO message_mutations(message_id,user_id,operation,from_folder,to_folder,status,error,created_at,desired_read,source_uid,source_uid_validity)
 SELECT m.id,a.user_id,'mark_read',m.folder,m.folder,'queued','',?, ?,COALESCE(NULLIF(?,0),m.imap_uid),COALESCE(NULLIF(?,0),m.uid_validity)
 FROM mail_messages m JOIN accounts a ON a.id=m.account_id WHERE m.id=? AND a.user_id=? AND m.account_id=? AND m.folder=? AND m.path=?
 AND (?=0 OR COALESCE(m.imap_uid,0)=?) AND (?=0 OR COALESCE(m.uid_validity,0)=?)`, time.Now().UTC().Format(time.RFC3339Nano), read, item.UID, item.UIDValidity, item.ID, userID, item.AccountID, item.Folder, item.Path, item.UID, item.UID, item.UIDValidity, item.UIDValidity)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return sql.ErrNoRows
	}
	return nil
}

// RecentReadMutations returns recent mark_read changes regardless of their
// journal status. Reconciliation replays provider flags once a queued change
// has been handed to mbsync, but the provider's own flag view can still lag
// behind that hand-off; a change this recent must not be erased by the replay.
func (s *Store) RecentReadMutations(ctx context.Context, accountID string, since string) ([]QueuedReadMutation, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT q.id,m.id,m.account_id,m.folder,m.path,COALESCE(m.message_id,'') AS message_id,COALESCE(q.desired_read,m.is_read),COALESCE(NULLIF(q.source_uid,0),m.imap_uid,0),COALESCE(NULLIF(q.source_uid_validity,0),m.uid_validity,0),m.is_read
 FROM message_mutations q JOIN mail_messages m ON m.id=q.message_id
 WHERE m.account_id=? AND q.operation='mark_read' AND q.status IN ('queued','synchronized','success') AND q.created_at>=?
 ORDER BY q.id`, accountID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var pending []QueuedReadMutation
	for rows.Next() {
		var item QueuedReadMutation
		if err := rows.Scan(&item.MutationID, &item.ID, &item.AccountID, &item.Folder, &item.Path, &item.MessageID, &item.Read, &item.UID, &item.UIDValidity, &item.CacheRead); err != nil {
			return nil, err
		}
		pending = append(pending, item)
	}
	return pending, rows.Err()
}
