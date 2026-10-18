package mailsync

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/voxmail/voxmail/internal/store"
)

func TestFailedRunInBackoffFallsBackToStartedAt(t *testing.T) {
	started := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	// A failed run whose FinishedAt is garbage must still honor the backoff
	// window measured from StartedAt, instead of retrying every tick.
	run := store.SyncRun{Success: false, StartedAt: started.Format(time.RFC3339Nano), FinishedAt: "not-a-timestamp"}
	if !failedRunInBackoff(run, started.Add(30*time.Second)) {
		t.Fatal("unparseable FinishedAt defeated the backoff")
	}
	if failedRunInBackoff(run, started.Add(2*syncRetryBackoff)) {
		t.Fatal("backoff never lifted")
	}
	// A parseable FinishedAt keeps working.
	run.FinishedAt = started.Format(time.RFC3339Nano)
	if !failedRunInBackoff(run, started.Add(30*time.Second)) {
		t.Fatal("parseable FinishedAt lost its backoff")
	}
}

func TestNextKindTransientStoreFailureFallsBackToIncremental(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	service := &Service{Store: db}
	if kind := service.nextKind(ctx, store.Account{ID: "fresh"}); kind != "initial" {
		t.Fatalf("fresh account kind=%q, want initial", kind)
	}
	// Close the underlying database: LatestSyncRun now fails with something
	// that is not ErrNoRows, and the kind must not degrade to "initial",
	// which would reapply the initial cutoff prune.
	db.Close()
	if kind := service.nextKind(ctx, store.Account{ID: "fresh"}); kind != "incremental" {
		t.Fatalf("kind after store failure=%q, want incremental", kind)
	}
}
