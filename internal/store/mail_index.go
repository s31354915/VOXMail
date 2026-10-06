package store

import (
	"context"
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
}

func (s *Store) UpsertMailIndex(ctx context.Context, input MailIndexInput) (int64, error) {
	if strings.TrimSpace(input.AccountID) == "" || strings.TrimSpace(input.Path) == "" {
		return 0, fmt.Errorf("account and mail path are required")
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO mail_messages(account_id,folder,path,message_id,sender,recipients,cc,subject,message_date,message_date_utc,is_read,attachment_count,updated_at,last_seen_scan,source_size) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(path) DO UPDATE SET folder=excluded.folder,message_id=excluded.message_id,sender=excluded.sender,recipients=excluded.recipients,cc=excluded.cc,subject=excluded.subject,message_date=excluded.message_date,message_date_utc=excluded.message_date_utc,is_read=excluded.is_read,attachment_count=excluded.attachment_count,updated_at=excluded.updated_at,last_seen_scan=excluded.last_seen_scan,source_size=excluded.source_size`, input.AccountID, input.Folder, input.Path, input.MessageID, input.Sender, input.Recipients, input.Cc, input.Subject, input.Date, input.DateUTC, input.Read, input.Attachments, input.UpdatedAt, input.ScanID, input.SourceSize)
	if err != nil {
		return 0, err
	}
	var id int64
	if err := s.DB.QueryRowContext(ctx, `SELECT id FROM mail_messages WHERE path=?`, input.Path).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

func (s *Store) PurgeUnseenMailIndex(ctx context.Context, accountID, scanID, pathPattern string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM mail_messages WHERE account_id=? AND last_seen_scan<>? AND path LIKE ? ESCAPE '\'`, accountID, scanID, pathPattern)
	return err
}
