package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/voxmail/voxmail/internal/secret"
	"github.com/voxmail/voxmail/internal/store"
)

type cancelCleanupAfterChecks struct {
	context.Context
	checks int
}

func (c *cancelCleanupAfterChecks) Err() error {
	c.checks--
	if c.checks <= 0 {
		return context.Canceled
	}
	return nil
}

func TestCleanupCancellationDuringLargeTreeIsResumable(t *testing.T) {
	srv, path := cleanupAccountFixture(t)
	for i := 0; i < 300; i++ {
		if err := os.WriteFile(filepath.Join(path, fmt.Sprintf("message-%03d", i)), []byte("private"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx := &cancelCleanupAfterChecks{Context: context.Background(), checks: 40}
	if err := srv.removeOwnedPath(ctx, path); !errors.Is(err, context.Canceled) {
		t.Fatal("ignored traversal cancellation", err)
	}
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) == 0 || len(entries) >= 301 {
		t.Fatalf("expected partially removed tree, got %d: %v", len(entries), err)
	}
	if err := srv.removeOwnedPath(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("partial cleanup did not resume", err)
	}
}

func TestUserDeletionCoordinatesHiddenInterruptedAccounts(t *testing.T) {
	srv, path := cleanupAccountFixture(t)
	if _, err := srv.Store.BeginAccountDeletionWithPaths(context.Background(), "user", "cleanup-account", []string{path}); err != nil {
		t.Fatal(err)
	}
	probe := &deletionSyncProbe{db: srv.Store}
	srv.Sync = probe
	response := voiceRequest(srv.Handler(), "admin", "DELETE", "/api/v1/users/user", nil)
	if response.Code != http.StatusAccepted {
		t.Fatalf("delete=%d %s", response.Code, response.Body.String())
	}
	if len(probe.stopped) == 0 || probe.stopped[0] != "cleanup-account" || !probe.deletingSeen {
		t.Fatal("hidden account was not coordinated", probe)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("user request performed cleanup", err)
	}
	if err := srv.cleanupAccountPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("deleted user artifacts survived", err)
	}
}

func cleanupAccountFixture(t *testing.T) (*Server, string) {
	t.Helper()
	srv, _ := voiceWebFixture(t)
	srv.DataRoot = t.TempDir()
	box, err := secret.New("cleanup-fixture-key-with-more-than-32-characters")
	if err != nil {
		t.Fatal(err)
	}
	a := store.Account{ID: "cleanup-account", UserID: "user", CanonicalName: "Mail", Email: "u@example.test", IMAPHost: "imap.example.test", IMAPUser: "u", IMAPPassword: "p", SMTPHost: "smtp.example.test", SMTPUser: "u", SMTPPassword: "p", FolderMap: `{}`}
	if err := srv.Store.SaveAccount(context.Background(), box, a); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(srv.DataRoot, "mail", a.ID)
	if err := os.MkdirAll(filepath.Join(path, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "nested", "message"), []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	return srv, path
}

func TestAccountCleanupWaitsForCoordinationAndResumesAfterRestart(t *testing.T) {
	srv, path := cleanupAccountFixture(t)
	ctx := context.Background()
	job, err := srv.Store.BeginAccountDeletionWithPaths(ctx, "user", "cleanup-account", []string{path})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.cleanupAccountPass(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("cleanup ran before coordination", err)
	}
	ids, err := srv.Store.AccountIDsForDeletion(ctx, "user")
	if err != nil || len(ids) != 1 {
		t.Fatalf("hidden deleting account omitted: %v %v", ids, err)
	}
	if err := srv.Store.FinalizeAccountDeletion(ctx, "user", "cleanup-account"); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := srv.cleanupAccountPass(canceled); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	var status string
	if err := srv.Store.DB.QueryRow(`SELECT status FROM cleanup_jobs WHERE id=?`, job.ID).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("lost canceled cleanup: %s %v", status, err)
	}
	// Close the application and database, then reopen the same durable file.
	var seq int
	var databaseName, databasePath string
	if err := srv.Store.DB.QueryRow(`PRAGMA database_list`).Scan(&seq, &databaseName, &databasePath); err != nil {
		t.Fatal(err)
	}
	srv.Close()
	if err := srv.Store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	restarted := &Server{Store: reopened, DataRoot: srv.DataRoot}
	if err := restarted.cleanupAccountPass(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("cleanup did not resume", err)
	}
}

func TestAccountCleanupConfinesPathsAndDoesNotFollowSymlinks(t *testing.T) {
	srv, path := cleanupAccountFixture(t)
	outside := t.TempDir()
	private := filepath.Join(outside, "private")
	if err := os.WriteFile(private, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(path, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{srv.DataRoot, filepath.Join(srv.DataRoot, "mail"), private, filepath.Join(srv.DataRoot, "mail", "..", "models")} {
		if err := srv.removeOwnedPath(context.Background(), bad); err == nil {
			t.Fatalf("accepted unsafe path %q", bad)
		}
	}
	if err := srv.removeOwnedPath(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(private); err != nil || string(data) != "keep" {
		t.Fatal("followed symlink", err)
	}
	if err := os.Remove(filepath.Join(srv.DataRoot, "mail")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(srv.DataRoot, "mail")); err != nil {
		t.Fatal(err)
	}
	if err := srv.removeOwnedPath(context.Background(), filepath.Join(srv.DataRoot, "mail", "private")); err == nil {
		t.Fatal("allowed root symlink escape")
	}
	if _, err := os.Stat(private); err != nil {
		t.Fatal("escaped configured tree", err)
	}
}

func TestCleanupStatusOwnershipFailureAndRetry(t *testing.T) {
	srv, path := cleanupAccountFixture(t)
	ctx := context.Background()
	job, err := srv.Store.BeginAccountDeletionWithPaths(ctx, "user", "cleanup-account", []string{path, filepath.Join(t.TempDir(), "outside")})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Store.FinalizeAccountDeletion(ctx, "user", "cleanup-account"); err != nil {
		t.Fatal(err)
	}
	if err := srv.cleanupAccountPass(ctx); err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()
	response := voiceRequest(h, "user", "GET", "/api/v1/cleanup-jobs", nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status":"failed"`) || strings.Contains(response.Body.String(), "outside") || strings.Contains(response.Body.String(), srv.DataRoot) {
		t.Fatalf("unsafe status: %d %s", response.Code, response.Body.String())
	}
	if _, err := srv.Store.DB.Exec(`UPDATE cleanup_jobs SET user_id='deleted-user' WHERE id=?`, job.ID); err != nil {
		t.Fatal(err)
	}
	response = voiceRequest(h, "user", "GET", "/api/v1/cleanup-jobs", nil)
	if strings.Contains(response.Body.String(), job.ID) {
		t.Fatal("foreign status disclosed")
	}
	response = voiceRequest(h, "user", "POST", "/api/v1/cleanup-jobs/"+job.ID+"/retry", nil)
	if response.Code != http.StatusNotFound {
		t.Fatal("foreign retry permitted", response.Code)
	}
	response = voiceRequest(h, "admin", "POST", "/api/v1/cleanup-jobs/"+job.ID+"/retry", nil)
	if response.Code != http.StatusAccepted {
		t.Fatal("admin recovery failed", response.Code, response.Body.String())
	}
	// Fix the artifact list as an operator would after correcting storage.
	paths, _ := json.Marshal([]string{path})
	if _, err := srv.Store.DB.Exec(`UPDATE cleanup_jobs SET paths=? WHERE id=?`, string(paths), job.ID); err != nil {
		t.Fatal(err)
	}
	if err := srv.cleanupAccountPass(ctx); err != nil {
		t.Fatal(err)
	}
	response = voiceRequest(h, "admin", "GET", "/api/v1/cleanup-jobs", nil)
	if !strings.Contains(response.Body.String(), `"status":"complete"`) {
		t.Fatal(response.Body.String())
	}
}
