package store

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
)

// Concurrent scans of the same folder previously interleaved the identity
// lookup, re-home decision, and insert as separate autocommit statements,
// so two indexers could both insert a row for the same message under
// different paths. The transactional upsert must converge on one row.
func TestUpsertMailIndexConcurrentSameMessageStaysOneRow(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedReadStateAccount(t, db, "a13", "u13")
	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			path := "/race-" + string(rune('a'+n)) + ":2,"
			_, err := db.UpsertMailIndex(ctx, MailIndexInput{
				AccountID: "a13", Folder: "Inbox", Path: path,
				MessageID: "<race@example.com>", Subject: "Hi",
				UpdatedAt: "t1", ScanID: "s1", SourceSize: 10,
				PathIsLive: func(string) bool { return false },
			})
			if err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var count int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM mail_messages WHERE message_id='<race@example.com>'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected exactly one row after concurrent upserts, got %d", count)
	}
}
