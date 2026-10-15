package mailsync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/voxmail/voxmail/internal/mailindex"
	"github.com/voxmail/voxmail/internal/mailparse"
	"github.com/voxmail/voxmail/internal/secret"
	"github.com/voxmail/voxmail/internal/store"
)

func TestFailedSyncIndexesDownloadedMailWithoutReconciliation(t *testing.T) {
	for _, mode := range []string{"timeout", "failure", "index-failure", "download-canceled", "index-canceled", "restart-offline"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			db, err := store.Open(filepath.Join(t.TempDir(), "mail.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := db.CreateUser(ctx, store.User{ID: "u", Username: "u", PasswordHash: "p", PINHash: "p", Role: "user", Enabled: true}); err != nil {
				t.Fatal(err)
			}
			box, err := secret.New("partial-sync-test-key-with-more-than-32-characters")
			if err != nil {
				t.Fatal(err)
			}
			cutoff := "2026-01-01T00:00:00Z"
			retention := 1
			account := store.Account{ID: "a", UserID: "u", CanonicalName: "Mail", Email: "u@example.com", SenderName: "User", IMAPHost: "imap.example", IMAPUser: "u", IMAPPassword: "p", SMTPHost: "smtp.example", SMTPUser: "u", SMTPPassword: "p", FolderMap: `{}`, InitialCutoff: &cutoff, RetentionDays: &retention}
			if mode == "index-canceled" {
				account.InitialCutoff = nil
				account.RetentionDays = nil
			}
			if err := db.SaveAccount(ctx, box, account); err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			mailRoot := filepath.Join(root, "mail", account.ID)
			folder := filepath.Join(mailRoot, "Inbox", "new")
			if err := os.MkdirAll(folder, 0700); err != nil {
				t.Fatal(err)
			}
			previousPath := filepath.Join(folder, "previous")
			if err := os.WriteFile(previousPath, []byte("From: sender@example.com\nSubject: previous\n\nPrevious body"), 0600); err != nil {
				t.Fatal(err)
			}
			indexer := &mailindex.Indexer{Store: db}
			if err := indexer.Index(ctx, account.ID, mailRoot); err != nil {
				t.Fatal(err)
			}
			previous, err := db.ListMail(ctx, "u", false)
			if err != nil || len(previous) != 1 {
				t.Fatalf("previous=%+v err=%v", previous, err)
			}
			if err := db.RecordMessageMutation(ctx, previous[0].ID, "u", "mark_read", "Inbox", "Inbox", "queued", nil); err != nil {
				t.Fatal(err)
			}
			script := `#!/bin/sh
set -eu
root=$(awk '$1 == "Path" {gsub(/"/, "", $2); print $2; exit}' "$2")
rm "$root/Inbox/new/previous"
printf '%s\n' 'From: sender@example.com' 'To: u@example.com' 'Subject: downloaded before failure' 'Message-ID: <partial@example.com>' 'Date: Thu, 01 Jan 2020 00:00:00 +0000' '' 'Downloaded body remains readable.' > "$root/Inbox/new/downloaded"
`
			if mode == "timeout" || mode == "download-canceled" {
				script += "exec sleep 30\n"
			} else if mode == "index-canceled" {
				script += "exit 0\n"
			} else {
				script += "echo 'fixture connection lost' >&2\nexit 7\n"
			}
			binary := filepath.Join(t.TempDir(), "mbsync")
			if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			runner := Runner{Binary: binary}
			if mode == "timeout" {
				runner.Timeout = 500 * time.Millisecond
			}
			service := &Service{Store: db, Root: root, Runner: runner, Index: indexer, EndpointResolver: func(context.Context, string, int) (string, error) { return "8.8.8.8:993", nil }}
			indexFailure := errors.New("fixture index unavailable")
			if mode == "index-failure" {
				service.Index = &mailindex.Indexer{Store: unavailableIndexStore{IndexStore: db, err: indexFailure}}
			}
			if mode == "index-canceled" {
				service.Index = &mailindex.Indexer{Store: &cancelOnceIndexStore{IndexStore: db, cancel: cancel}}
			}
			if mode == "download-canceled" {
				go func(watchCtx context.Context) {
					timer := time.NewTimer(3 * time.Second)
					defer timer.Stop()
					ticker := time.NewTicker(10 * time.Millisecond)
					defer ticker.Stop()
					for {
						select {
						case <-watchCtx.Done():
							return
						case <-timer.C:
							cancel()
							return
						case <-ticker.C:
							if _, err := os.Stat(filepath.Join(folder, "downloaded")); err == nil {
								cancel()
								return
							}
						}
					}
				}(ctx)
			}

			if mode == "restart-offline" {
				if _, err := db.BeginSyncRun(ctx, account.ID, "manual"); err != nil {
					t.Fatal(err)
				}
				if _, err := db.InterruptSyncRuns(ctx); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(previousPath); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(folder, "downloaded"), []byte("From: sender@example.com\nSubject: downloaded before failure\n\nDownloaded body remains readable."), 0600); err != nil {
					t.Fatal(err)
				}
				service.EndpointResolver = func(ctx context.Context, _ string, _ int) (string, error) {
					messages, err := db.ListMail(ctx, "u", false)
					if err != nil || len(messages) != 2 {
						t.Errorf("local recovery must finish before network access: messages=%d err=%v", len(messages), err)
					}
					return "", errors.New("fixture connection lost")
				}
			}
			// A file still in Maildir tmp is an incomplete download, not an email.
			tmpDir := filepath.Join(mailRoot, "Inbox", "tmp")
			if err := os.MkdirAll(tmpDir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(tmpDir, "unfinished"), []byte("Subject: incomplete\n\npartial body"), 0600); err != nil {
				t.Fatal(err)
			}
			err = service.SyncAll(ctx)
			// Inspect durable recovery using a live context after caller cancellation.
			ctx = context.Background()
			if (mode == "download-canceled" || mode == "index-canceled") && !errors.Is(err, context.Canceled) {
				t.Fatalf("error=%v, want cancellation", err)
			}
			if mode == "index-failure" {
				if !errors.Is(err, indexFailure) || !strings.Contains(err.Error(), "fixture connection lost") {
					t.Fatalf("error=%v, want both runner and index failures", err)
				}
				run, lookupErr := db.LatestSyncRun(ctx, account.ID, false)
				if lookupErr != nil || run.Success || !strings.Contains(run.Error, "fixture index unavailable") || !strings.Contains(run.Error, "fixture connection lost") {
					t.Fatalf("durable failures=%+v err=%v", run, lookupErr)
				}
				return
			}
			if mode == "timeout" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("error=%v, want deadline", err)
			}
			if (mode == "failure" || mode == "restart-offline") && (err == nil || !strings.Contains(err.Error(), "fixture connection lost")) {
				t.Fatalf("error=%v, want original failure", err)
			}
			messages, err := db.ListMail(ctx, "u", false)
			if err != nil || len(messages) != 2 {
				t.Fatalf("messages=%+v err=%v, want downloaded and retained previous row", messages, err)
			}
			found := false
			for _, message := range messages {
				if message.Subject != "downloaded before failure" {
					continue
				}
				found = true
				file, err := os.Open(message.Path)
				if err != nil {
					t.Fatal(err)
				}
				parsed, err := mailparse.Parse(file)
				file.Close()
				if err != nil || !strings.Contains(parsed.Text, "Downloaded body remains readable.") {
					t.Fatalf("body=%q err=%v", parsed.Text, err)
				}
			}
			if !found {
				t.Fatal("downloaded message not accessible")
			}
			run, err := db.LatestSyncRun(ctx, account.ID, false)
			if err != nil || run.Success || run.FinishedAt == "" || run.Error == "" {
				t.Fatalf("failed run=%+v err=%v", run, err)
			}
			if _, err := db.LatestFullSyncRun(ctx, account.ID); err == nil {
				t.Fatal("failed partial import counted as successful full synchronization")
			}
			mutations, err := db.ListMessageMutations(ctx, "u", account.ID, 10)
			if err != nil || len(mutations) != 1 || mutations[0].Status != "queued" {
				t.Fatalf("mutations=%+v err=%v", mutations, err)
			}
			// A later complete scan still removes the missing previous file's row.
			if err := indexer.Index(ctx, account.ID, mailRoot); err != nil {
				t.Fatal(err)
			}
			messages, err = db.ListMail(ctx, "u", false)
			if err != nil || len(messages) != 1 {
				t.Fatalf("complete scan messages=%+v err=%v", messages, err)
			}
		})
	}
}

// Inject only the lookup boundary; all other indexing operations remain real.
type unavailableIndexStore struct {
	mailindex.IndexStore
	err error
}

func (s unavailableIndexStore) FindMailIndex(context.Context, string, string) (store.MailIndexRecord, error) {
	return store.MailIndexRecord{}, s.err
}

func TestPartialIndexHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := (mailindex.Indexer{}).IndexAvailable(ctx, "a", t.TempDir())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want cancellation before store access", err)
	}
}

// Cancel the first indexing attempt after mbsync has returned successfully.
// The recovery scan must use a fresh context and retry that same file.
type cancelOnceIndexStore struct {
	mailindex.IndexStore
	cancel context.CancelFunc
}

func (s *cancelOnceIndexStore) FindMailIndex(ctx context.Context, account, path string) (store.MailIndexRecord, error) {
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
		return store.MailIndexRecord{}, ctx.Err()
	}
	return s.IndexStore.FindMailIndex(ctx, account, path)
}
