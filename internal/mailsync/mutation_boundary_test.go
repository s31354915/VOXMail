package mailsync

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/voxmail/voxmail/internal/secret"
	"github.com/voxmail/voxmail/internal/store"
)

func TestSyncAcknowledgesOnlyMutationsQueuedBeforeRunnerStarts(t *testing.T) {
	for _, fails := range []bool{false, true} {
		name := "success"
		if fails {
			name = "failure"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			db, err := store.Open(filepath.Join(t.TempDir(), "mail.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := db.CreateUser(ctx, store.User{ID: "u", Username: "u", PasswordHash: "p", PINHash: "p", Role: "user", Enabled: true}); err != nil {
				t.Fatal(err)
			}
			box, err := secret.New("mutation-sync-test-key-with-more-than-32-characters")
			if err != nil {
				t.Fatal(err)
			}
			account := store.Account{ID: "a", UserID: "u", CanonicalName: "Mail", Email: "u@example.com", SenderName: "User", IMAPHost: "imap.example", IMAPUser: "u", IMAPPassword: "p", SMTPHost: "smtp.example", SMTPUser: "u", SMTPPassword: "p", FolderMap: `{}`}
			if err := db.SaveAccount(ctx, box, account); err != nil {
				t.Fatal(err)
			}
			accounts, err := db.ListAccounts(ctx, "u")
			if err != nil || len(accounts) != 1 {
				t.Fatalf("accounts=%v err=%v", accounts, err)
			}
			account = accounts[0]
			result, err := db.DB.ExecContext(ctx, `INSERT INTO mail_messages(account_id,folder,path,updated_at) VALUES('a','Inbox','/fixture/message','now')`)
			if err != nil {
				t.Fatal(err)
			}
			messageID, err := result.LastInsertId()
			if err != nil {
				t.Fatal(err)
			}
			queue := func() {
				t.Helper()
				if err := db.RecordMessageMutation(ctx, messageID, "u", "mark_read", "Inbox", "Inbox", "queued", nil); err != nil {
					t.Fatal(err)
				}
			}
			queue()
			fixture := t.TempDir()
			runnerPath := filepath.Join(fixture, "mbsync")
			// All fixture filenames are fixed and resolved relative to the executable.
			script := "#!/bin/sh\nset -eu\ncd \"$(dirname \"$0\")\"\ntouch started\nwhile [ ! -f release ]; do sleep 0.01; done\n"
			if fails {
				script += "exit 7\n"
			}
			if err := os.WriteFile(runnerPath, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			service := &Service{Store: db, Root: t.TempDir(), Runner: Runner{Binary: runnerPath}, EndpointResolver: func(context.Context, string, int) (string, error) { return "8.8.8.8:993", nil }}
			done := make(chan error, 1)
			go func() { done <- service.syncAccount(ctx, account, "incremental") }()
			for {
				if _, err := os.Stat(filepath.Join(fixture, "started")); err == nil {
					break
				}
				select {
				case err := <-done:
					t.Fatalf("runner stopped before marker: %v", err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(time.Millisecond):
				}
			}
			queue()
			if err := os.WriteFile(filepath.Join(fixture, "release"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if (err != nil) != fails {
					t.Fatalf("sync error=%v fails=%v", err, fails)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			items, err := db.ListMessageMutations(ctx, "u", "a", 10)
			if err != nil || len(items) != 2 {
				t.Fatalf("journal=%v err=%v", items, err)
			}
			if items[0].Status != "queued" {
				t.Fatalf("late mutation falsely acknowledged: %+v", items[0])
			}
			want := "synchronized"
			if fails {
				want = "queued"
			}
			if items[1].Status != want {
				t.Fatalf("pre-run mutation=%+v want %s", items[1], want)
			}
		})
	}
}
