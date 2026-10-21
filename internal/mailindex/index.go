package mailindex

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/voxmail/voxmail/internal/mailparse"
	"github.com/voxmail/voxmail/internal/store"
)

type IndexStore interface {
	// ListMailIndexStamps loads the whole (path, size, mtime) projection in
	// one query so the scan diffs in memory instead of issuing one lookup
	// per maildir file.
	ListMailIndexStamps(context.Context, string) (map[string]store.MailIndexStamp, error)
	// MarkMailIndexSeenBatch bulk-marks unchanged paths with the scan id.
	MarkMailIndexSeenBatch(context.Context, string, string, []string) (int64, error)
	FindMailIndex(context.Context, string, string) (store.MailIndexRecord, error)
	MarkMailIndexSeen(context.Context, string, string, string) error
	UpsertMailIndex(context.Context, store.MailIndexInput) (int64, error)
	ReplaceAttachmentMetadata(context.Context, int64, []store.AttachmentMetadata) error
	PurgeUnseenMailIndex(context.Context, string, string, string) error
}

type Indexer struct {
	Store IndexStore
	Log   *slog.Logger
}

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
	return i.index(ctx, accountID, root, true)
}

// IndexAvailable imports local messages after an incomplete synchronization.
// Missing files are not proof of remote deletion, so existing rows are retained.
func (i Indexer) IndexAvailable(ctx context.Context, accountID, root string) error {
	return i.index(ctx, accountID, root, false)
}

func (i Indexer) index(ctx context.Context, accountID, root string, purge bool) error {
	// Fail fast on a context that is already cancelled, before any store
	// access: the stamp load is the scan's first query and the walk would
	// otherwise start against a dead context.
	if err := ctx.Err(); err != nil {
		return err
	}
	scanID := newScanID()
	modTime := func(info os.FileInfo) string { return info.ModTime().UTC().Format(time.RFC3339Nano) }
	// One stamp query per scan replaces one FindMailIndex query per file:
	// the walk diffs against the loaded map and unchanged paths are
	// batch-marked after the walk. The map is bounded by the account's
	// indexed message count.
	stamps, err := i.Store.ListMailIndexStamps(ctx, accountID)
	if err != nil {
		return err
	}
	unchanged := make([]string, 0, len(stamps))
	err = mailparse.ScanMetadataEachContextWithFilterAndErrors(ctx, root, func(path string, info os.FileInfo) (bool, error) {
		stamp, ok := stamps[path]
		if !ok {
			return true, nil
		}
		if stamp.SourceSize != info.Size() || stamp.UpdatedAt != modTime(info) {
			return true, nil
		}
		unchanged = append(unchanged, path)
		return false, nil
	}, func(message mailparse.MaildirMessage) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		return i.record(ctx, accountID, scanID, message)
	}, func(path string, parseErr error) error {
		logger := i.Log
		if logger == nil {
			logger = slog.Default()
		}
		if _, exists := stamps[path]; exists {
			// Preserve the previous record for this still-present message. Otherwise
			// the successful end-of-scan purge would mistake an unparseable file for
			// a deleted message and remove its last usable metadata. The stamp map
			// was loaded at scan start, so it answers for the record the strict
			// parse just failed on.
			logger.Warn("mail index message failed strict parse; keeping existing record", "account_id", accountID, "path", path, "error", parseErr)
			unchanged = append(unchanged, path)
			return nil
		}
		// A never-indexed file gets one lenient recovery attempt before the
		// scan gives up on it: a truncated body, corrupt base64, or bare-LF
		// file the strict parser rejects may still hold a readable
		// projection. Files that already carry valid metadata keep it.
		if recovered, ok := i.lenientMessage(accountID, root, path); ok {
			logger.Info("mail index recovered damaged message via lenient parse", "account_id", accountID, "path", path)
			return i.record(ctx, accountID, scanID, recovered)
		}
		// The message is only "skipped" once every recovery option is
		// exhausted, so the warning names files the index truly lost. A
		// damaged file re-logs this on every scan cycle, so warn once per
		// path until the dedup map recycles.
		if noteIndexSkip(accountID, path) {
			logger.Warn("mail index skipped malformed message", "account_id", accountID, "path", path, "error", parseErr)
		}
		return nil
	})
	if err != nil {
		return err
	}
	// The end-of-scan purge deletes every row the scan did not mark, so the
	// batch mark must fully succeed before it runs. A failed batch returns
	// before the purge, the same protection the per-file mark path had.
	if len(unchanged) > 0 {
		if _, err := i.Store.MarkMailIndexSeenBatch(ctx, accountID, scanID, unchanged); err != nil {
			return err
		}
	}
	if !purge {
		return nil
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

// record upserts one parsed message and its attachment metadata. It backs
// both the normal visit path and the lenient recovery path.
func (i Indexer) record(ctx context.Context, accountID, scanID string, message mailparse.MaildirMessage) error {
	info, err := os.Stat(message.Path)
	if err != nil {
		return nil
	}
	messageID, err := i.Store.UpsertMailIndex(ctx, store.MailIndexInput{AccountID: accountID, Folder: message.Folder, Path: message.Path, MessageID: message.MessageID, Sender: message.From, Recipients: message.To, Cc: message.Cc, Subject: message.Subject, Date: message.Date, DateUTC: store.NormalizeMessageDate(message.Date), Read: message.Read, Attachments: len(message.Attachments), UpdatedAt: info.ModTime().UTC().Format(time.RFC3339Nano), ScanID: scanID, SourceSize: info.Size(), PathIsLive: func(candidate string) bool { _, statErr := os.Stat(candidate); return statErr == nil }})
	if err != nil {
		return err
	}
	metadata := make([]store.AttachmentMetadata, 0, len(message.Attachments))
	for index, attachment := range message.Attachments {
		metadata = append(metadata, store.AttachmentMetadata{PartPath: fmt.Sprintf("part-%d", index+1), Filename: attachment.Name, ContentType: attachment.ContentType, Size: attachment.Size, ContentID: attachment.ContentID, Disposition: attachment.Disposition, Playable: attachment.Playable})
	}
	return i.Store.ReplaceAttachmentMetadata(ctx, messageID, metadata)
}

// lenientMessage re-opens a failed file and parses it with the lenient
// recovery path, deriving the same folder and read-flag projection the scan
// would have produced.
func (i Indexer) lenientMessage(accountID, root, path string) (mailparse.MaildirMessage, bool) {
	_ = accountID
	file, err := os.Open(path)
	if err != nil {
		return mailparse.MaildirMessage{}, false
	}
	defer file.Close()
	message, err := mailparse.ParseLenientMetadata(file)
	if err != nil {
		return mailparse.MaildirMessage{}, false
	}
	folderPath := filepath.Dir(filepath.Dir(path))
	folder, relErr := filepath.Rel(root, folderPath)
	if relErr != nil || folder == "." || strings.HasPrefix(folder, ".."+string(filepath.Separator)) {
		return mailparse.MaildirMessage{}, false
	}
	folder = filepath.ToSlash(folder)
	name := filepath.Base(path)
	read := false
	if marker := strings.Index(name, ":2,"); marker >= 0 {
		read = strings.Contains(name[marker+3:], "S")
	}
	return mailparse.MaildirMessage{Path: path, Folder: folder, Read: read, Message: message}, true
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

var (
	indexSkipWarnMu   sync.Mutex
	indexSkipWarnSeen = map[string]struct{}{}
)

// noteIndexSkip returns true the first time a given malformed file is
// reported for an account, so a permanently damaged file warns once instead
// of once per scan cycle. The map recycles when it grows large.
func noteIndexSkip(accountID, path string) bool {
	indexSkipWarnMu.Lock()
	defer indexSkipWarnMu.Unlock()
	key := accountID + "\x00" + path
	if _, ok := indexSkipWarnSeen[key]; ok {
		return false
	}
	if len(indexSkipWarnSeen) >= 4096 {
		indexSkipWarnSeen = map[string]struct{}{}
	}
	indexSkipWarnSeen[key] = struct{}{}
	return true
}
