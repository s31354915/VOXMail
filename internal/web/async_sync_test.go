package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/voxmail/voxmail/internal/mailsync"
	"github.com/voxmail/voxmail/internal/secret"
	"github.com/voxmail/voxmail/internal/store"
)

func TestAsyncManualSyncSurvivesRequestAndJoinsOnShutdown(t *testing.T) {
	for _, cancelMode := range []string{"api", "shutdown"} {
		t.Run(cancelMode, func(t *testing.T) {
			srv, h := voiceWebFixture(t)
			box, err := secret.New("async-sync-fixture-key-with-more-than-32-characters")
			if err != nil {
				t.Fatal(err)
			}
			account := store.Account{ID: "async-mail", UserID: "user", CanonicalName: "Mail", Email: "u@example.test", IMAPHost: "imap.example.test", IMAPUser: "u", IMAPPassword: "p", SMTPHost: "smtp.example.test", SMTPUser: "u", SMTPPassword: "p", FolderMap: `{}`}
			if err := srv.Store.SaveAccount(context.Background(), box, account); err != nil {
				t.Fatal(err)
			}
			entered, canceled, release := make(chan context.Context, 1), make(chan struct{}), make(chan struct{})
			var once sync.Once
			releaseWorker := func() { once.Do(func() { close(release) }) }
			defer releaseWorker()
			service := &mailsync.Service{Store: srv.Store, Root: t.TempDir(), EndpointResolver: func(ctx context.Context, _ string, _ int) (string, error) {
				entered <- ctx
				<-ctx.Done()
				close(canceled)
				<-release // cleanup must finish before Close returns or the DB closes
				return "", ctx.Err()
			}}
			srv.Sync = service
			requestCtx, disconnect := context.WithCancel(context.Background())
			defer disconnect()
			request := httptest.NewRequest("POST", "/api/v1/accounts/async-mail/sync", nil).WithContext(requestCtx)
			request.AddCookie(&http.Cookie{Name: "voxmail_session", Value: "user-token"})
			request.Header.Set("X-CSRF-Token", "user-csrf")
			response := httptest.NewRecorder()
			returned := make(chan struct{})
			go func() { h.ServeHTTP(response, request); close(returned) }()
			select {
			case <-returned:
			case <-time.After(2 * time.Second):
				t.Fatal("HTTP request waited for mailbox sync")
			}
			if response.Code != http.StatusAccepted || response.Header().Get("Location") != "/api/v1/accounts" {
				t.Fatalf("response=%d %s", response.Code, response.Body.String())
			}
			var result struct {
				RunID int64 `json:"run_id"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.RunID == 0 {
				t.Fatalf("missing run ID: %s", response.Body.String())
			}
			var worker context.Context
			select {
			case worker = <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("worker never began")
			}
			if _, deadline := worker.Deadline(); deadline {
				t.Fatal("large mailbox received implicit deadline")
			}
			disconnect()
			if worker.Err() != nil || !service.SyncRunning(account.ID) {
				t.Fatal("request disconnect canceled admitted job")
			}
			run, err := srv.Store.LatestSyncRun(context.Background(), account.ID, false)
			if err != nil || run.ID != result.RunID || run.FinishedAt != "" {
				t.Fatalf("accepted run was not durable: %+v %v", run, err)
			}
			duplicate := voiceRequest(h, "user", "POST", "/api/v1/accounts/async-mail/sync", nil)
			if duplicate.Code != http.StatusConflict {
				t.Fatalf("duplicate=%d %s", duplicate.Code, duplicate.Body.String())
			}
			foreign := voiceRequest(h, "admin", "POST", "/api/v1/accounts/async-mail/sync", nil)
			if foreign.Code != http.StatusNotFound {
				t.Fatal("non-owner admitted sync")
			}
			if cancelMode == "api" {
				cancelResponse := voiceRequest(h, "user", "POST", "/api/v1/accounts/async-mail/sync/cancel", nil)
				if cancelResponse.Code != http.StatusAccepted {
					t.Fatalf("cancel=%d %s", cancelResponse.Code, cancelResponse.Body.String())
				}
			}
			closed := make(chan struct{})
			go func() { srv.Close(); close(closed) }()
			select {
			case <-canceled:
			case <-time.After(2 * time.Second):
				t.Fatal("job not canceled")
			}
			select {
			case <-closed:
				t.Fatal("Close abandoned job cleanup")
			default:
			}
			releaseWorker()
			select {
			case <-closed:
			case <-time.After(2 * time.Second):
				t.Fatal("Close did not join job")
			}
			run, err = srv.Store.LatestSyncRun(context.Background(), account.ID, false)
			if err != nil || run.ID != result.RunID || run.FinishedAt == "" || run.Success || run.ErrorCategory != "canceled" {
				t.Fatalf("bad terminal job: %+v %v", run, err)
			}
			if service.SyncRunning(account.ID) {
				t.Fatal("completed job remained active")
			}
			rows, err := srv.Store.ListSyncRuns(context.Background(), "user", account.ID, 20)
			if err != nil || len(rows) != 1 {
				t.Fatalf("duplicate admitted durable work: %v %v", rows, err)
			}
			if strings.Contains(response.Body.String(), "Password") {
				t.Fatal("job response leaked secrets")
			}
		})
	}
}
