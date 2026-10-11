package mailsync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/voxmail/voxmail/internal/secret"
	"github.com/voxmail/voxmail/internal/store"
)

// BenchmarkSequentialAccountDelay exercises the real scheduler, SQLite run
// records, generated configs and bounded child execution. A disposable child
// models a slow failing provider without making outbound connections. Measure
// time to the middle healthy-speed account as well as total sweep time, and
// place the slow account before/after it to distinguish ordering from overhead.
func BenchmarkSequentialAccountDelay(b *testing.B) {
	for _, scheduler := range []string{"startup", "due"} {
		for _, tc := range []struct {
			name   string
			slowID int
			delay  time.Duration
		}{
			{"baseline", 0, 0}, {"slow_first_50ms", 0, 50 * time.Millisecond}, {"slow_last_50ms", 2, 50 * time.Millisecond},
			{"slow_first_200ms", 0, 200 * time.Millisecond}, {"slow_last_200ms", 2, 200 * time.Millisecond},
		} {
			b.Run(scheduler+"/"+tc.name, func(b *testing.B) {
				ctx := context.Background()
				db, err := store.Open(filepath.Join(b.TempDir(), "benchmark.db"))
				if err != nil {
					b.Fatal(err)
				}
				defer db.Close()
				if err := db.CreateUser(ctx, store.User{ID: "bench-user", Username: "bench-user", PasswordHash: "p", PINHash: "p", Role: "user", Enabled: true}); err != nil {
					b.Fatal(err)
				}
				box, err := secret.New("scheduling-benchmark-disposable-key-0123456789")
				if err != nil {
					b.Fatal(err)
				}
				for i := 0; i < 3; i++ {
					a := store.Account{ID: fmt.Sprintf("bench-%d", i), UserID: "bench-user", CanonicalName: fmt.Sprintf("Account %d", i), DisplayOrder: i, Email: "bench@example.test", IMAPHost: fmt.Sprintf("bench-%d.example.test", i), IMAPUser: "bench", IMAPPassword: "p", SMTPHost: "smtp.example.test", SMTPUser: "bench", SMTPPassword: "p", FolderMap: `{}`}
					if err := db.SaveAccount(ctx, box, a); err != nil {
						b.Fatal(err)
					}
				}
				binary := filepath.Join(b.TempDir(), "slow-provider-fixture")
				script := fmt.Sprintf("#!/bin/sh\ncase \"$2\" in */bench-%d.conf) sleep %.3f ;; esac\nexit 7\n", tc.slowID, tc.delay.Seconds())
				if tc.delay == 0 {
					script = "#!/bin/sh\nexit 7\n"
				}
				if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
					b.Fatal(err)
				}
				var started time.Time
				var middleDelay time.Duration
				service := &Service{Store: db, Root: b.TempDir(), Runner: Runner{Binary: binary}, now: func() time.Time { return time.Now().Add(time.Hour) }, EndpointResolver: func(_ context.Context, host string, _ int) (string, error) {
					if host == "bench-1.example.test" {
						middleDelay += time.Since(started)
					}
					return "192.0.2.1:993", nil
				}}
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					started = time.Now()
					var err error
					if scheduler == "startup" {
						err = service.SyncAll(ctx)
					} else {
						err = service.SyncDue(ctx)
					}
					if err == nil {
						b.Fatal("fixture did not exercise failed sync completion")
					}
					var child *exec.ExitError
					if !errors.As(err, &child) || child.ExitCode() != 7 {
						b.Fatalf("fixture failed before executing the provider child: %v", err)
					}
				}
				b.StopTimer()
				b.ReportMetric(float64(middleDelay.Nanoseconds())/float64(b.N)/1e6, "middle-start-ms/op")
				var runs int
				if err := db.DB.QueryRow(`SELECT COUNT(*) FROM sync_runs`).Scan(&runs); err != nil || runs != 3*b.N {
					b.Fatalf("scheduler skipped work: runs=%d want=%d err=%v", runs, 3*b.N, err)
				}
			})
		}
	}
}
