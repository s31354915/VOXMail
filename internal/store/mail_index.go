package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// IndexedMail is the bounded identity projection needed by reconciliation.
// Keeping this projection in the store prevents synchronization code from
// reaching through the database handle for implementation-specific columns.
type IndexedMail struct {
	ID          int64
	Path        string
	Folder      string
	MessageID   string
	UID         uint32
	UIDValidity uint32
}

func (s *Store) ListIndexedMail(ctx context.Context, accountID string) ([]IndexedMail, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,path,folder,COALESCE(message_id,''),COALESCE(imap_uid,0),COALESCE(uid_validity,0) FROM mail_messages WHERE account_id=?`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IndexedMail
	for rows.Next() {
		var item IndexedMail
		if err := rows.Scan(&item.ID, &item.Path, &item.Folder, &item.MessageID, &item.UID, &item.UIDValidity); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) UpdateIndexedMailUID(ctx context.Context, accountID string, id int64, uid, uidValidity uint32) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE mail_messages SET imap_uid=?,uid_validity=?,updated_at=? WHERE id=? AND account_id=?`, uid, uidValidity, time.Now().UTC().Format(time.RFC3339Nano), id, accountID)
	return err
}

func (s *Store) DeleteIndexedMail(ctx context.Context, accountID string, id int64) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM mail_messages WHERE id=? AND account_id=?`, id, accountID)
	return err
}

type MailIndexRecord struct {
	SourceSize int64
	UpdatedAt  string
}

func (s *Store) FindMailIndex(ctx context.Context, accountID, path string) (MailIndexRecord, error) {
	var record MailIndexRecord
	err := s.DB.QueryRowContext(ctx, `SELECT source_size,updated_at FROM mail_messages WHERE account_id=? AND path=?`, accountID, path).Scan(&record.SourceSize, &record.UpdatedAt)
	return record, err
}

func (s *Store) MarkMailIndexSeen(ctx context.Context, accountID, path, scanID string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE mail_messages SET last_seen_scan=? WHERE account_id=? AND path=?`, scanID, accountID, path)
	return err
}

// MailIndexStamp is the lightweight (path, size, mtime) projection the index
// scan diffs against the maildir.
type MailIndexStamp struct {
	SourceSize int64
	UpdatedAt  string
}

// ListMailIndexStamps loads every indexed path for an account in one query so
// a scan can diff the maildir in memory instead of issuing one lookup per
// file. The result's size is bounded by the account's indexed message count.
func (s *Store) ListMailIndexStamps(ctx context.Context, accountID string) (map[string]MailIndexStamp, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT path,COALESCE(source_size,0),COALESCE(updated_at,'') FROM mail_messages WHERE account_id=?`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	stamps := make(map[string]MailIndexStamp)
	for rows.Next() {
		var path string
		var stamp MailIndexStamp
		if err := rows.Scan(&path, &stamp.SourceSize, &stamp.UpdatedAt); err != nil {
			return nil, err
		}
		stamps[path] = stamp
	}
	return stamps, rows.Err()
}

// markMailIndexSeenBatchChunkSize bounds each batched update's parameter
// count, staying well inside SQLite's host-parameter limit.
const markMailIndexSeenBatchChunkSize = 500

// MarkMailIndexSeenBatch marks every given path as seen by scanID in chunked
// bulk updates and returns the number of rows updated. A path with no row is
// not an error: the end-of-scan purge keeps relying on every live row
// carrying the current scan id.
func (s *Store) MarkMailIndexSeenBatch(ctx context.Context, accountID, scanID string, paths []string) (int64, error) {
	var updated int64
	for start := 0; start < len(paths); start += markMailIndexSeenBatchChunkSize {
		end := start + markMailIndexSeenBatchChunkSize
		if end > len(paths) {
			end = len(paths)
		}
		chunk := paths[start:end]
		query := `UPDATE mail_messages SET last_seen_scan=? WHERE account_id=? AND path IN (` + strings.Repeat("?,", len(chunk)-1) + "?)"
		args := make([]any, 0, len(chunk)+2)
		args = append(args, scanID, accountID)
		for _, path := range chunk {
			args = append(args, path)
		}
		result, err := s.DB.ExecContext(ctx, query, args...)
		if err != nil {
			return updated, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return updated, err
		}
		updated += count
	}
	return updated, nil
}

type MailIndexInput struct {
	AccountID   string
	Folder      string
	Path        string
	MessageID   string
	Sender      string
	Recipients  string
	Cc          string
	Subject     string
	Date        string
	DateUTC     *int64
	Read        bool
	Attachments int
	UpdatedAt   string
	ScanID      string
	SourceSize  int64
	// Alerted and AlertAttempts carry durable alert state from an existing
	// row for the same message identity. UpsertMailIndex populates them when
	// a message re-indexes under a new path so a sync cannot reset the
	// announced mark or the alert retry cap.
	Alerted       int
	AlertAttempts int
	// PathIsLive reports whether a candidate row's stored path still exists
	// on disk. The indexer supplies it from the maildir itself. A re-home
	// onto a new path is only allowed for a row whose file is gone, so a
	// folder holding genuine duplicate copies of the same message keeps one
	// row per copy instead of collapsing onto the last indexed copy.
	PathIsLive func(string) bool
}

func (s *Store) UpsertMailIndex(ctx context.Context, input MailIndexInput) (int64, error) {
	if strings.TrimSpace(input.AccountID) == "" || strings.TrimSpace(input.Path) == "" {
		return 0, fmt.Errorf("account and mail path are required")
	}
	// The identity lookup, the re-home decision, and the final insert must
	// run atomically. Previously each step was its own autocommit
	// statement, so two scans of the same folder could interleave: both
	// observed "no stale row and no row at this path" and both inserted,
	// leaving two rows for the same message under different paths. The
	// duplicate survives until the next scan's re-home pass and, in the
	// meantime, surfaces as an always-unread, unplayable entry.
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	// A message can re-index under a new path (flag rename, re-download,
	// UID churn). Carry the announced mark and retry attempts from any
	// existing row for the same message identity so a sync cannot reset
	// them; without this the same message alerted again after every sync.
	if input.PathIsLive != nil || strings.TrimSpace(input.MessageID) != "" {
		patterns := maildirBaseLike(maildirBaseName(input.Path))
		var alerted, attempts int
		err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(alerted),0), COALESCE(MAX(alert_attempts),0) FROM mail_messages WHERE account_id=? AND folder=? AND path<>? AND ((?<>'' AND message_id=?) OR path LIKE ? ESCAPE '\' OR path LIKE ? ESCAPE '\' OR path LIKE ? ESCAPE '\')`, input.AccountID, input.Folder, input.Path, input.MessageID, input.MessageID, patterns[0], patterns[1], patterns[2]).Scan(&alerted, &attempts)
		if err == nil {
			if alerted != 0 || attempts > 0 {
				input.Alerted, input.AlertAttempts = alerted, attempts
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
	}

	// A message that re-indexes under a new path (flag rename, re-download,
	// UID churn) must re-home its existing row instead of inserting a second
	// row for the same message. Duplicates under stale paths surface as
	// unplayable, always-unread messages in the call list until a purge scan
	// removes them, and a row inserted for the renamed file collides with the
	// concurrent mark_read path update on UNIQUE(path). Identity is the
	// Message-ID header when the message carries one, and otherwise the
	// immutable Maildir base filename, which a new/ to cur/:2,S rename keeps.
	// Only rows whose file no longer exists on disk count as stale: a folder
	// can legitimately hold several copies of the same message, and every
	// live copy needs its own row.
	if input.PathIsLive != nil {
		patterns := maildirBaseLike(maildirBaseName(input.Path))
		candidateRows, err := tx.QueryContext(ctx, `SELECT id,path,is_read FROM mail_messages WHERE account_id=? AND folder=? AND path<>? AND NOT EXISTS(SELECT 1 FROM mail_messages other WHERE other.path=?) AND ((?<>'' AND message_id=?) OR path LIKE ? ESCAPE '\' OR path LIKE ? ESCAPE '\' OR path LIKE ? ESCAPE '\') ORDER BY updated_at DESC`, input.AccountID, input.Folder, input.Path, input.Path, input.MessageID, input.MessageID, patterns[0], patterns[1], patterns[2])
		if err != nil {
			return 0, err
		}
		type staleCandidate struct {
			id   int64
			path string
			read bool
		}
		var stale []staleCandidate
		for candidateRows.Next() {
			var candidate staleCandidate
			if err := candidateRows.Scan(&candidate.id, &candidate.path, &candidate.read); err != nil {
				candidateRows.Close()
				return 0, err
			}
			if !input.PathIsLive(candidate.path) {
				stale = append(stale, candidate)
			}
		}
		candidateRows.Close()
		if err := candidateRows.Err(); err != nil {
			return 0, err
		}
		if len(stale) > 0 {
			moveID := stale[0].id
			deadRead := input.Read
			for _, candidate := range stale {
				if candidate.read {
					deadRead = true
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE mail_messages SET path=?,message_id=CASE WHEN TRIM(?)<>'' THEN ? ELSE message_id END,sender=?,recipients=?,cc=?,subject=?,message_date=?,message_date_utc=?,is_read=?,alerted=?,alert_attempts=?,attachment_count=?,updated_at=?,last_seen_scan=?,source_size=? WHERE id=?`, input.Path, input.MessageID, input.MessageID, input.Sender, input.Recipients, input.Cc, input.Subject, input.Date, input.DateUTC, deadRead, input.Alerted, input.AlertAttempts, input.Attachments, input.UpdatedAt, input.ScanID, input.SourceSize, moveID); err != nil {
				return 0, err
			}
			for _, candidate := range stale[1:] {
				if _, err := tx.ExecContext(ctx, `DELETE FROM mail_messages WHERE id=? AND account_id=?`, candidate.id, input.AccountID); err != nil {
					return 0, err
				}
			}
			if err := tx.Commit(); err != nil {
				return 0, err
			}
			return moveID, nil
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO mail_messages(account_id,folder,path,message_id,sender,recipients,cc,subject,message_date,message_date_utc,is_read,alerted,alert_attempts,attachment_count,updated_at,last_seen_scan,source_size) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(path) DO UPDATE SET folder=excluded.folder,message_id=excluded.message_id,sender=excluded.sender,recipients=excluded.recipients,cc=excluded.cc,subject=excluded.subject,message_date=excluded.message_date,message_date_utc=excluded.message_date_utc,is_read=excluded.is_read,attachment_count=excluded.attachment_count,updated_at=excluded.updated_at,last_seen_scan=excluded.last_seen_scan,source_size=excluded.source_size`, input.AccountID, input.Folder, input.Path, input.MessageID, input.Sender, input.Recipients, input.Cc, input.Subject, input.Date, input.DateUTC, input.Read, input.Alerted, input.AlertAttempts, input.Attachments, input.UpdatedAt, input.ScanID, input.SourceSize); err != nil {
		return 0, err
	}
	var id int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM mail_messages WHERE path=?`, input.Path).Scan(&id); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

func (s *Store) PurgeUnseenMailIndex(ctx context.Context, accountID, scanID, pathPattern string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM mail_messages WHERE account_id=? AND last_seen_scan<>? AND path LIKE ? ESCAPE '\'`, accountID, scanID, pathPattern)
	return err
}
