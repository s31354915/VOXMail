package mailindex

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/voxmail/voxmail/internal/mailparse"
	"github.com/voxmail/voxmail/internal/store"
)

type IndexStore interface {
	FindMailIndex(context.Context, string, string) (store.MailIndexRecord, error)
	MarkMailIndexSeen(context.Context, string, string, string) error
	UpsertMailIndex(context.Context, store.MailIndexInput) (int64, error)
	ReplaceAttachmentMetadata(context.Context, int64, []store.AttachmentMetadata) error
	PurgeUnseenMailIndex(context.Context, string, string, string) error
}

type Indexer struct{ Store IndexStore }

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
	removed := false
	err := mailparse.ScanMetadataEach(root, func(message mailparse.MaildirMessage) error {
		date, err := mail.ParseDate(message.Date)
		if err != nil {
			return nil
		}
		tooOld := cutoff != nil && date.Before(*cutoff)
		if !tooOld && !retentionCutoff.IsZero() {
			tooOld = date.Before(retentionCutoff)
		}
		if !tooOld || !underRoot(message.Path, root) {
			return nil
		}
		if err := os.Remove(message.Path); err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		removed = true
		return nil
	})
	if err != nil {
		return removed, err
	}
	return removed, nil
}

func (i Indexer) Index(ctx context.Context, accountID, root string) error {
	scanID := newScanID()
	modTime := func(info os.FileInfo) string { return info.ModTime().UTC().Format(time.RFC3339Nano) }
	err := mailparse.ScanMetadataEachContextWithFilter(ctx, root, func(path string, info os.FileInfo) (bool, error) {
		record, err := i.Store.FindMailIndex(ctx, accountID, path)
		if errors.Is(err, sql.ErrNoRows) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if record.SourceSize != info.Size() || record.UpdatedAt != modTime(info) {
			return true, nil
		}
		return false, i.Store.MarkMailIndexSeen(ctx, accountID, path, scanID)
	}, func(message mailparse.MaildirMessage) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		info, err := os.Stat(message.Path)
		if err != nil {
			return nil
		}
		messageID, err := i.Store.UpsertMailIndex(ctx, store.MailIndexInput{AccountID: accountID, Folder: message.Folder, Path: message.Path, MessageID: message.MessageID, Sender: message.From, Recipients: message.To, Cc: message.Cc, Subject: message.Subject, Date: message.Date, DateUTC: store.NormalizeMessageDate(message.Date), Read: message.Read, Attachments: len(message.Attachments), UpdatedAt: modTime(info), ScanID: scanID, SourceSize: info.Size()})
		if err != nil {
			return err
		}
		metadata := make([]store.AttachmentMetadata, 0, len(message.Attachments))
		for index, attachment := range message.Attachments {
			metadata = append(metadata, store.AttachmentMetadata{PartPath: fmt.Sprintf("part-%d", index+1), Filename: attachment.Name, ContentType: attachment.ContentType, Size: attachment.Size, ContentID: attachment.ContentID, Disposition: attachment.Disposition, Playable: attachment.Playable})
		}
		if err := i.Store.ReplaceAttachmentMetadata(ctx, messageID, metadata); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	return i.purgeStale(ctx, accountID, root, scanID)
}

// purgeStale removes index rows whose maildir file no longer exists. Mail that
// was deleted on the remote and removed by mbsync would otherwise linger in
// the index forever, surfacing in folders and alerts as dead entries.
func (i Indexer) purgeStale(ctx context.Context, accountID, root, scanID string) error {
	prefix := filepath.Clean(root) + string(os.PathSeparator)
	pattern := strings.ReplaceAll(prefix, `\`, `\\`)
	pattern = strings.ReplaceAll(pattern, `%`, `\%`)
	pattern = strings.ReplaceAll(pattern, `_`, `\_`) + "%"
	return i.Store.PurgeUnseenMailIndex(ctx, accountID, scanID, pattern)
}

func newScanID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err == nil {
		return hex.EncodeToString(raw[:])
	}
	return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
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
