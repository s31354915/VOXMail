package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
)

// maildirBaseName extracts the immutable base identifier of a Maildir file
// name: the final path segment up to the ":2," flag separator. A read flag
// change renames new/<base> to cur/<base>:2,S (mbsync may add an ",S=size"
// info field before the flags), so both names map to the same base. The base
// identifies the physical file even when the message carries no Message-ID
// header.
func maildirBaseName(path string) string {
	name := path
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	// The unique name ends at the first ":" (flag section) or "," (info
	// field such as mbsync's ",S=size"), so both a plain new/ name and its
	// flagged cur/ copy map to the same base.
	cut := strings.IndexByte(name, ':')
	if c := strings.IndexByte(name, ','); c >= 0 && (cut < 0 || c < cut) {
		cut = c
	}
	if cut >= 0 {
		name = name[:cut]
	}
	return name
}

// maildirFlagsRead reports whether a Maildir filename's flag section marks
// the message seen ("S" after ":2,"). Files in new/ carry no flags and are
// always unread.
func maildirFlagsRead(path string) bool {
	i := strings.Index(path, ":2,")
	if i < 0 {
		return false
	}
	return strings.Contains(path[i+3:], "S")
}

func escapeLikePattern(value string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(value)
}

// maildirBaseLike returns the three LIKE patterns that match every possible
// name the base can grow into: unchanged (new/ copy), an appended info field
// (",S=size"), or an appended flag section (":2,FLAGS"). Matching on the
// exact delimiters keeps a shorter base from inheriting the state of an
// unrelated longer filename that merely shares the prefix.
func maildirBaseLike(base string) []string {
	escaped := escapeLikePattern(base)
	return []string{"%/" + escaped, "%/" + escaped + ",%", "%/" + escaped + ":%"}
}

// mergeMailPathCollision resolves a UNIQUE(path) collision before a mutation
// moves a message's indexed path. The background indexer indexes a renamed
// Maildir file under its new path while the original row still holds the old
// one, so a later mark_read path update used to crash with "UNIQUE constraint
// failed: mail_messages.path", roll the whole mutation back, and leave the
// message permanently stuck unread. The collision row describes the same
// physical message whenever the two Message-IDs agree or either is unknown,
// so it is merged into the row the mutation anchors on (alert marks, retry
// attempts and a known Message-ID survive) and removed. Two different
// non-empty Message-IDs on one path mean genuinely distinct messages claim
// the same file, and the caller gets a descriptive conflict instead of a raw
// constraint error.
func (s *Store) mergeMailPathCollision(ctx context.Context, tx *sql.Tx, accountID string, rowID int64, newPath string) error {
	var collideID int64
	var collideMessageID string
	err := tx.QueryRowContext(ctx, `SELECT id,COALESCE(message_id,'') FROM mail_messages WHERE account_id=? AND path=? AND id<>?`, accountID, newPath, rowID).Scan(&collideID, &collideMessageID)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	var rowMessageID string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(message_id,'') FROM mail_messages WHERE id=? AND account_id=?`, rowID, accountID).Scan(&rowMessageID); err != nil {
		return err
	}
	if rowMessageID != "" && collideMessageID != "" && rowMessageID != collideMessageID {
		return fmt.Errorf("maildir path %q is already indexed for a different message", newPath)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE mail_messages SET
 alerted=MAX(alerted,IFNULL((SELECT alerted FROM mail_messages WHERE id=?),0)),
 alert_attempts=MAX(alert_attempts,IFNULL((SELECT alert_attempts FROM mail_messages WHERE id=?),0)),
 message_id=CASE WHEN COALESCE(message_id,'')='' THEN IFNULL((SELECT message_id FROM mail_messages WHERE id=?),'') ELSE message_id END
 WHERE id=?`, collideID, collideID, collideID, rowID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM mail_messages WHERE id=? AND account_id=?`, collideID, accountID); err != nil {
		return err
	}
	return nil
}

// mergeMaildirPathDuplicates collapses duplicate mail_messages rows that
// reference the same physical Maildir message: same account, same folder, and
// the same immutable base filename. The flag rename (new/ to cur/:2,S) used
// to be indexed as a brand-new row while the original row kept its stale
// path, so every later read toggle failed on the UNIQUE(path) constraint and
// the affected messages stayed unread forever. Each duplicate group merges
// into the row that still matches the Maildir on disk (the flagged cur/ copy
// wins ties), carries the stronger alert state and any known Message-ID
// across, and deletes the stale rows. The cleanup is idempotent and runs at
// startup next to the message-date backfill.
func (s *Store) mergeMaildirPathDuplicates(ctx context.Context) error {
	type indexedRow struct {
		id        int64
		path      string
		messageID string
		alerted   int
		attempts  int
		updatedAt string
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT id,account_id,folder,path,COALESCE(message_id,''),COALESCE(alerted,0),COALESCE(alert_attempts,0),COALESCE(updated_at,'') FROM mail_messages ORDER BY account_id,folder,id`)
	if err != nil {
		return fmt.Errorf("scan mail index for duplicates: %w", err)
	}
	defer rows.Close()
	groups := map[string][]indexedRow{}
	for rows.Next() {
		var id int64
		var accountID, folder, path, messageID, updatedAt string
		var alerted, attempts int
		if err := rows.Scan(&id, &accountID, &folder, &path, &messageID, &alerted, &attempts, &updatedAt); err != nil {
			return err
		}
		key := accountID + "\x00" + folder + "\x00" + maildirBaseName(path)
		if maildirBaseName(path) == "" {
			continue
		}
		groups[key] = append(groups[key], indexedRow{id: id, path: path, messageID: messageID, alerted: alerted, attempts: attempts, updatedAt: updatedAt})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	for _, group := range groups {
		if len(group) < 2 {
			continue
		}
		survivor := group[0]
		// A row whose file still exists on disk wins, then the flagged cur/
		// copy, then the freshest updated_at.
		rank := func(row indexedRow) int {
			live := 0
			if _, err := os.Stat(row.path); err == nil {
				live = 2
			}
			flagged := 0
			if strings.Contains(row.path, ":2,") {
				flagged = 1
			}
			return live*2 + flagged
		}
		for _, row := range group[1:] {
			rowRank, survivorRank := rank(row), rank(survivor)
			if rowRank > survivorRank || (rowRank == survivorRank && row.updatedAt > survivor.updatedAt) {
				survivor = row
			}
		}
		for _, row := range group {
			if row.id == survivor.id {
				continue
			}
			if _, err := s.DB.ExecContext(ctx, `UPDATE mail_messages SET
 alerted=MAX(alerted,?),
 alert_attempts=MAX(alert_attempts,?),
 message_id=CASE WHEN COALESCE(message_id,'')='' THEN ? ELSE message_id END
 WHERE id=?`, row.alerted, row.attempts, row.messageID, survivor.id); err != nil {
				return fmt.Errorf("merge mail index duplicate into %d: %w", survivor.id, err)
			}
			if _, err := s.DB.ExecContext(ctx, `DELETE FROM mail_messages WHERE id=?`, row.id); err != nil {
				return fmt.Errorf("delete stale mail index row %d: %w", row.id, err)
			}
		}
	}
	return nil
}
