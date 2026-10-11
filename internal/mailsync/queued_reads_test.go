package mailsync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	remoteimap "github.com/voxmail/voxmail/internal/imap"
	"github.com/voxmail/voxmail/internal/mailindex"
	"github.com/voxmail/voxmail/internal/store"
)

type readAttempt struct {
	folder     string
	uid, epoch uint32
	read       bool
}
type retryReadConnection struct {
	attempts     *[]readAttempt
	writeErr     error
	state        remoteimap.MutationState
	reconcileErr error
	reconciled   *int
	closed       bool
}

func (c *retryReadConnection) SetSeenByUID(folder string, uid, epoch uint32, read bool) error {
	*c.attempts = append(*c.attempts, readAttempt{folder, uid, epoch, read})
	return c.writeErr
}
func (c *retryReadConnection) ReconcileSeenByUID(folder string, uid, epoch uint32, read bool) (remoteimap.MutationState, error) {
	*c.reconciled++
	return c.state, c.reconcileErr
}
func (c *retryReadConnection) Close() error { c.closed = true; return nil }

func queuedRetainedFixture(t *testing.T) (*store.Store, store.Account, []store.QueuedReadMutation) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.DB.Exec(`INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('u','u','p','p','user','now');
 INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a','u','Mail','u@e','User','imap','u','p','smtp','u','p','{"Alias":"NewAlias"}','now');
 INSERT INTO mail_messages(id,account_id,folder,path,message_id,is_read,imap_uid,uid_validity,updated_at) VALUES(1,'a','OldAlias','/old','<same@test>',1,7,101,'now'),(2,'a','OldAlias','/other','<same@test>',0,8,101,'now');`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := db.UpsertRemoteFolder(ctx, "a", "Alias", "OldAlias", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertRemoteFolder(ctx, "a", "Alias", "NewAlias", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordMessageMutation(ctx, 1, "u", "mark_read", "OldAlias", "OldAlias", "queued", nil); err != nil {
		t.Fatal(err)
	}
	watermark, err := db.QueuedMutationWatermark(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	pending, err := db.QueuedReadMutations(ctx, "a", watermark)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending=%v err=%v", pending, err)
	}
	return db, store.Account{ID: "a", UserID: "u"}, pending
}

func TestRetainedReadRetryUsesExactSourceUIDAndPreservesFailedQueue(t *testing.T) {
	for _, tc := range []struct {
		name     string
		writeErr error
		state    remoteimap.MutationState
		wantErr  error
		opens    int
	}{
		{name: "confirmed", opens: 1},
		{name: "changed epoch", writeErr: remoteimap.ErrUIDValidityChanged, wantErr: remoteimap.ErrUIDValidityChanged, opens: 1},
		{name: "lost response confirmed", writeErr: remoteimap.ErrMutationUncertain, state: remoteimap.MutationStateApplied, opens: 2},
		{name: "lost response not applied", writeErr: remoteimap.ErrMutationUncertain, state: remoteimap.MutationStateNotApplied, wantErr: remoteimap.ErrMutationNotApplied, opens: 2},
		{name: "lost response unknown", writeErr: remoteimap.ErrMutationUncertain, state: remoteimap.MutationStateUnknown, wantErr: remoteimap.ErrMutationUncertain, opens: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, account, pending := queuedRetainedFixture(t)
			var attempts []readAttempt
			opens, reconciled := 0, 0
			var connections []*retryReadConnection
			open := func() (readMutationConnection, error) {
				opens++
				c := &retryReadConnection{attempts: &attempts, reconciled: &reconciled, writeErr: tc.writeErr, state: tc.state}
				connections = append(connections, c)
				return c, nil
			}
			err := (&Service{Store: db}).confirmRetainedReads(context.Background(), account, pending, open)
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) || tc.wantErr == nil && err != nil {
				t.Fatalf("retry error=%v want %v", err, tc.wantErr)
			}
			if opens != tc.opens || len(attempts) != 1 || attempts[0] != (readAttempt{"Alias", 7, 101, true}) {
				t.Fatalf("opens=%d attempts=%+v", opens, attempts)
			}
			for _, c := range connections {
				if !c.closed {
					t.Error("retry connection not closed")
				}
			}
			items, err := db.ListMessageMutations(context.Background(), "u", "a", 10)
			if err != nil || len(items) != 1 {
				t.Fatalf("journal=%v err=%v", items, err)
			}
			want := "synchronized"
			if tc.wantErr != nil {
				want = "queued"
				if items[0].Error == "" {
					t.Error("missing retry diagnostic")
				}
			}
			if items[0].Status != want {
				t.Fatalf("status=%s want %s", items[0].Status, want)
			}
			var otherRead bool
			if err := db.DB.QueryRow(`SELECT is_read FROM mail_messages WHERE id=2`).Scan(&otherRead); err != nil || otherRead {
				t.Fatalf("distinct same-Message-ID copy changed: %v %v", otherRead, err)
			}
		})
	}
}

func TestRetainedReadRetryRefusesMissingIdentityWithoutOpeningProvider(t *testing.T) {
	db, account, pending := queuedRetainedFixture(t)
	if _, err := db.DB.Exec(`UPDATE mail_messages SET imap_uid=0 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	pending[0].UID = 0
	err := (&Service{Store: db}).confirmRetainedReads(context.Background(), account, pending, func() (readMutationConnection, error) { t.Fatal("missing UID must not open provider"); return nil, nil })
	if !errors.Is(err, remoteimap.ErrMissingIdentity) {
		t.Fatalf("error=%v", err)
	}
}

func TestRetainedReadConfirmationDoesNotAcknowledgeChangedDesiredState(t *testing.T) {
	db, account, pending := queuedRetainedFixture(t)
	var attempts []readAttempt
	reconciled := 0
	open := func() (readMutationConnection, error) {
		// Another call toggles unread while this older read attempt is in flight.
		if _, err := db.DB.Exec(`UPDATE mail_messages SET is_read=0 WHERE id=1`); err != nil {
			t.Fatal(err)
		}
		if err := db.RecordMessageMutation(context.Background(), 1, "u", "mark_read", "OldAlias", "OldAlias", "queued", nil); err != nil {
			t.Fatal(err)
		}
		return &retryReadConnection{attempts: &attempts, reconciled: &reconciled}, nil
	}
	if err := (&Service{Store: db}).confirmRetainedReads(context.Background(), account, pending, open); err != nil {
		t.Fatal(err)
	}
	items, err := db.ListMessageMutations(context.Background(), "u", "a", 10)
	if err != nil || len(items) != 2 {
		t.Fatalf("journal=%v err=%v", items, err)
	}
	for _, item := range items {
		if item.Status != "queued" {
			t.Fatalf("superseded attempt falsely acknowledged: %+v", item)
		}
	}
}

func TestRetainedReadRetryHonorsNewerActionAcrossCurrentCacheCopy(t *testing.T) {
	for _, completed := range []bool{false, true} {
		name := "pending newer current copy"
		if completed {
			name = "already synchronized current copy"
		}
		t.Run(name, func(t *testing.T) {
			db, account, pending := queuedRetainedFixture(t)
			ctx := context.Background()
			if _, err := db.DB.Exec(`INSERT INTO mail_messages(id,account_id,folder,path,message_id,is_read,imap_uid,uid_validity,updated_at) VALUES(3,'a','NewAlias','/current','<same@test>',0,7,101,'now')`); err != nil {
				t.Fatal(err)
			}
			if err := db.RecordMessageMutation(ctx, 3, "u", "mark_read", "NewAlias", "NewAlias", "queued", nil); err != nil {
				t.Fatal(err)
			}
			watermark, err := db.QueuedMutationWatermark(ctx, "a")
			if err != nil {
				t.Fatal(err)
			}
			if completed {
				if err := db.MarkQueuedMutationsSynchronized(ctx, "a", watermark, []string{"NewAlias"}); err != nil {
					t.Fatal(err)
				}
			}
			// A later cache refresh must not rewrite the durable requested flag.
			if _, err := db.DB.Exec(`UPDATE mail_messages SET is_read=1 WHERE id=3`); err != nil {
				t.Fatal(err)
			}
			var attempts []readAttempt
			reconciled := 0
			open := func() (readMutationConnection, error) {
				return &retryReadConnection{attempts: &attempts, reconciled: &reconciled}, nil
			}
			if err := (&Service{Store: db}).confirmRetainedReads(ctx, account, pending, open); err != nil {
				t.Fatal(err)
			}
			if len(attempts) != 1 || attempts[0] != (readAttempt{"Alias", 7, 101, false}) {
				t.Fatalf("older retained intent overrode newer current copy: %+v", attempts)
			}
			var retainedStatus string
			if err := db.DB.QueryRow(`SELECT status FROM message_mutations WHERE message_id=1`).Scan(&retainedStatus); err != nil || retainedStatus != "synchronized" {
				t.Fatalf("superseded retained queue=%s err=%v", retainedStatus, err)
			}
		})
	}
}

func TestRetainedReadRetryKeepsDifferentProviderUIDsIndependent(t *testing.T) {
	db, account, pending := queuedRetainedFixture(t)
	ctx := context.Background()
	// A newer action with the same Message-ID but a different UID is unrelated.
	if err := db.RecordMessageMutation(ctx, 2, "u", "mark_read", "OldAlias", "OldAlias", "queued", nil); err != nil {
		t.Fatal(err)
	}
	var attempts []readAttempt
	reconciled := 0
	if err := (&Service{Store: db}).confirmRetainedReads(ctx, account, pending, func() (readMutationConnection, error) {
		return &retryReadConnection{attempts: &attempts, reconciled: &reconciled}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0] != (readAttempt{"Alias", 7, 101, true}) {
		t.Fatalf("distinct UID affected ordering: %+v", attempts)
	}
}

func TestRetainedReadRetryAlignsOnlyExactIdentityCacheCopies(t *testing.T) {
	db, account, pending := queuedRetainedFixture(t)
	ctx := context.Background()
	root := t.TempDir()
	paths := map[int64]string{}
	for id, folder := range map[int64]string{1: "OldAlias", 2: "OldAlias", 3: "NewAlias", 4: "Other"} {
		dir := filepath.Join(root, "mail", "a", folder, "cur")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, fmt.Sprintf("copy-%d:2,S", id))
		if err := os.WriteFile(path, []byte("From: sender@example.test\nMessage-ID: <same@test>\n\nBody\n"), 0600); err != nil {
			t.Fatal(err)
		}
		paths[id] = path
	}
	for _, id := range []int64{1, 2} {
		if _, err := db.DB.Exec(`UPDATE mail_messages SET path=? WHERE id=?`, paths[id], id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.DB.Exec(`INSERT INTO mail_messages(id,account_id,folder,path,message_id,is_read,imap_uid,uid_validity,updated_at) VALUES(3,'a','NewAlias',?,'<same@test>',0,7,101,'now'),(4,'a','Other',?,'<same@test>',1,7,101,'now')`, paths[3], paths[4]); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertRemoteFolder(ctx, "a", "Unrelated", "Other", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordMessageMutation(ctx, 3, "u", "mark_read", "NewAlias", "NewAlias", "queued", nil); err != nil {
		t.Fatal(err)
	}
	// A stale index refresh presents the wrong current-copy flag.
	if _, err := db.DB.Exec(`UPDATE mail_messages SET is_read=1 WHERE id=3`); err != nil {
		t.Fatal(err)
	}
	var attempts []readAttempt
	reconciled := 0
	svc := &Service{Store: db, Root: root}
	if err := svc.confirmRetainedReads(ctx, account, pending, func() (readMutationConnection, error) {
		return &retryReadConnection{attempts: &attempts, reconciled: &reconciled}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].read {
		t.Fatalf("newer requested unread lost: %+v", attempts)
	}
	for _, id := range []int64{1, 3} {
		var path string
		var read bool
		if err := db.DB.QueryRow(`SELECT path,is_read FROM mail_messages WHERE id=?`, id).Scan(&path, &read); err != nil {
			t.Fatal(err)
		}
		if read || !strings.HasSuffix(path, ":2,") {
			t.Fatalf("copy %d has stale flag: %q read=%v", id, path, read)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
	// Different UID and different provider folder remain physically untouched.
	for _, id := range []int64{2, 4} {
		if _, err := os.Stat(paths[id]); err != nil {
			t.Fatalf("unrelated copy %d changed: %v", id, err)
		}
	}
	items, err := db.ListMessageMutations(ctx, "u", "a", 10)
	if err != nil || len(items) != 2 {
		t.Fatalf("journal=%v err=%v", items, err)
	}
	for _, item := range items {
		if item.Status != "synchronized" {
			t.Fatalf("old/current queue not settled: %+v", item)
		}
	}
	// Indexing and a later retry cannot restore the superseded flag.
	if err := (&mailindex.Indexer{Store: db}).Index(ctx, "a", filepath.Join(root, "mail", "a")); err != nil {
		t.Fatal(err)
	}
	var read bool
	if err := db.DB.QueryRow(`SELECT is_read FROM mail_messages WHERE id=1`).Scan(&read); err != nil || read {
		t.Fatalf("index restored obsolete flag: %v %v", read, err)
	}
}
