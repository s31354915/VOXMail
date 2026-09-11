package mailindex

import (
	"context"
	"fmt"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/voxmail/voxmail/internal/mailparse"
	"github.com/voxmail/voxmail/internal/store"
)

type Indexer struct{ Store *store.Store }

// PruneLocal applies the account's local retention policy. It intentionally
// removes only files below root; it never issues an IMAP delete or expunge.
// A message with an unparsable Date header is retained rather than guessed at.
func (i Indexer) PruneLocal(root string, cutoff *time.Time, retentionDays *int) (bool, error) {
	if root == "" || (cutoff == nil && (retentionDays == nil || *retentionDays <= 0)) {
		return false, nil
	}
	var retentionCutoff time.Time
	if retentionDays != nil && *retentionDays > 0 {
		retentionCutoff = time.Now().Add(-time.Duration(*retentionDays) * 24 * time.Hour)
	}
	messages, err := mailparse.Scan(root)
	if err != nil {
		return false, err
	}
	removed := false
	for _, message := range messages {
		date, err := mail.ParseDate(message.Date)
		if err != nil {
			continue
		}
		tooOld := cutoff != nil && date.Before(*cutoff)
		if !tooOld && !retentionCutoff.IsZero() {
			tooOld = date.Before(retentionCutoff)
		}
		if !tooOld || !underRoot(message.Path, root) {
			continue
		}
		if err := os.Remove(message.Path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return removed, err
		}
		removed = true
	}
	return removed, nil
}

func (i Indexer) Index(ctx context.Context, accountID, root string) error {
	messages, err := mailparse.Scan(root)
	if err != nil {
		return err
	}
	present := make(map[string]struct{}, len(messages))
	for _, message := range messages {
		info, err := os.Stat(message.Path)
		if err != nil {
			continue
		}
		present[message.Path] = struct{}{}
		_, err = i.Store.DB.ExecContext(ctx, `INSERT INTO mail_messages(account_id,folder,path,message_id,sender,recipients,cc,subject,message_date,is_read,attachment_count,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(path) DO UPDATE SET folder=excluded.folder,message_id=excluded.message_id,sender=excluded.sender,recipients=excluded.recipients,cc=excluded.cc,subject=excluded.subject,message_date=excluded.message_date,is_read=excluded.is_read,attachment_count=excluded.attachment_count,updated_at=excluded.updated_at`, accountID, message.Folder, message.Path, message.MessageID, message.From, message.To, message.Cc, message.Subject, message.Date, message.Read, len(message.Attachments), info.ModTime().UTC().Format(time.RFC3339Nano))
		if err != nil {
			return err
		}
		var messageID int64
		if err := i.Store.DB.QueryRowContext(ctx, `SELECT id FROM mail_messages WHERE path=?`, message.Path).Scan(&messageID); err != nil {
			return err
		}
		metadata := make([]store.AttachmentMetadata, 0, len(message.Attachments))
		for index, attachment := range message.Attachments {
			metadata = append(metadata, store.AttachmentMetadata{PartPath: fmt.Sprintf("part-%d", index+1), Filename: attachment.Name, ContentType: attachment.ContentType, Size: attachment.Size, ContentID: attachment.ContentID, Disposition: attachment.Disposition, Playable: attachment.Playable})
		}
		if err := i.Store.ReplaceAttachmentMetadata(ctx, messageID, metadata); err != nil {
			return err
		}
	}
	return i.purgeStale(ctx, accountID, root, present)
}

// purgeStale removes index rows whose maildir file no longer exists. Mail that
// was deleted on the remote and removed by mbsync would otherwise linger in
// the index forever, surfacing in folders and alerts as dead entries.
func (i Indexer) purgeStale(ctx context.Context, accountID, root string, present map[string]struct{}) error {
	rows, err := i.Store.DB.QueryContext(ctx, `SELECT path FROM mail_messages WHERE account_id=?`, accountID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var stale []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return err
		}
		if _, ok := present[path]; ok {
			continue
		}
		if !underRoot(path, root) {
			continue
		}
		stale = append(stale, path)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, path := range stale {
		if _, err := i.Store.DB.ExecContext(ctx, `DELETE FROM mail_messages WHERE account_id=? AND path=?`, accountID, path); err != nil {
			return err
		}
	}
	return nil
}

func underRoot(path, root string) bool {
	root = filepath.Clean(root)
	if root == "" {
		return false
	}
	if strings.HasPrefix(filepath.Clean(path), root+string(filepath.Separator)) {
		return true
	}
	return filepath.Clean(path) == root
}
