package mailsync

import (
	"context"
	"errors"
	"fmt"
	"github.com/voxmail/voxmail/internal/secret"
	"github.com/voxmail/voxmail/internal/store"
	"path/filepath"
	"testing"
)

func TestManualSyncAdmissionIsBoundedAndCapacityRecovers(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "async.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateUser(context.Background(), store.User{ID: "u", Username: "u", PasswordHash: "p", PINHash: "p", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	box, err := secret.New("async-capacity-key-with-more-than-32-characters")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= maxManualSyncJobs; i++ {
		a := store.Account{ID: fmt.Sprint(i), UserID: "u", CanonicalName: "Mail", Email: "u@example.test", IMAPHost: "imap.test", IMAPUser: "u", IMAPPassword: "p", SMTPHost: "smtp.test", SMTPUser: "u", SMTPPassword: "p", FolderMap: `{}`}
		if err := db.SaveAccount(context.Background(), box, a); err != nil {
			t.Fatal(err)
		}
	}
	service := &Service{Store: db, Root: t.TempDir(), EndpointResolver: func(ctx context.Context, _ string, _ int) (string, error) { <-ctx.Done(); return "", ctx.Err() }}
	worker, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); service.WaitManualSyncs() }()
	for i := 0; i < maxManualSyncJobs; i++ {
		id, err := service.StartRefreshAccount(context.Background(), worker, "u", fmt.Sprint(i))
		if err != nil || id == 0 {
			t.Fatalf("admission %d: id=%d err=%v", i, id, err)
		}
	}
	id, err := service.StartRefreshAccount(context.Background(), worker, "u", fmt.Sprint(maxManualSyncJobs))
	if id != 0 || !errors.Is(err, ErrSyncCapacity) {
		t.Fatalf("unbounded admission: id=%d err=%v", id, err)
	}
	runs, err := db.ListSyncRuns(context.Background(), "u", "", 20)
	if err != nil || len(runs) != maxManualSyncJobs {
		t.Fatalf("unexpected durable jobs: %d %v", len(runs), err)
	}
	cancel()
	service.WaitManualSyncs()
	next, nextCancel := context.WithCancel(context.Background())
	defer func() { nextCancel(); service.WaitManualSyncs() }()
	id, err = service.StartRefreshAccount(context.Background(), next, "u", fmt.Sprint(maxManualSyncJobs))
	if err != nil || id == 0 {
		t.Fatalf("capacity never recovered: id=%d err=%v", id, err)
	}
}
