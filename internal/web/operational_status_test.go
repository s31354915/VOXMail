package web

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/voxmail/voxmail/internal/operation"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/voxmail/voxmail/internal/mailsync"
	"github.com/voxmail/voxmail/internal/secret"
	"github.com/voxmail/voxmail/internal/store"
)

func TestOperationalFailureDoesNotEchoProviderOutput(t *testing.T) {
	for _, tc := range []struct{ signal, category string }{
		{"context deadline exceeded", "timeout"}, {"context canceled", "canceled"},
		{"AUTHENTICATIONFAILED", "authentication"}, {"x509 certificate failed", "tls"},
		{"no such host", "dns"}, {"connection refused", "connection"},
		{"no space left on device", "disk_full"}, {"permission denied", "storage_permissions"},
		{"executable file not found", "missing_dependency"}, {"UIDVALIDITY mismatch", "mail_index"},
		{"endpoint rejected by outbound policy", "endpoint_policy"},
		{"interrupted by application restart", "interrupted"},
		{"403 Forbidden", "authentication"}, {"504 Registration failure", "timeout"}, {"404 Not found", "sip_account"},
		{"500 Provider failure", "operation_failed"}, {"unknown provider output", "operation_failed"},
	} {
		t.Run(tc.category+"/"+tc.signal, func(t *testing.T) {
			failure := sipRegistrationFailure(tc.signal + " password=SensitivePassword token=SensitiveToken https://user:SensitivePassword@provider.test/private")
			if failure == nil || failure.Category != tc.category || failure.Reason == "" || failure.Action == "" {
				t.Fatalf("missing useful cause: %+v", failure)
			}
			encoded, _ := json.Marshal(failure)
			if strings.Contains(string(encoded), "Sensitive") || strings.Contains(string(encoded), "provider.test") {
				t.Fatalf("transcript leaked: %s", encoded)
			}
		})
	}
	if operationalFailure("") != nil || safeReadinessReason("") == "" {
		t.Fatal("empty reason handling failed")
	}
}

type statusFailureSync struct{}

func (statusFailureSync) RefreshAccount(context.Context, string, string) error {
	return operation.Wrap("fixture", "authenticate", operation.Authentication, errors.New("authentication failed password=SensitivePassword token=SensitiveToken"))
}

func TestOperationalSnapshotAndRealSyncStateTransitions(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "status.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	box, err := secret.New("disposable-operational-status-key-123456789")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	binary := filepath.Join(root, "mbsync-fixture")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\necho 'Authentication failed password=SensitivePassword token=SensitiveToken' >&2\nexit 7\n"), 0700); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	syncer := &mailsync.Service{Store: db, Root: root, Runner: mailsync.Runner{Binary: binary}, EndpointResolver: func(ctx context.Context, _ string, _ int) (string, error) {
		close(entered)
		select {
		case <-release:
			return "8.8.8.8:993", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}}
	runtime := RuntimeStatus{Process: "running", Registration: "failed", Bridge: "connected", Media: "ready", LastError: "timeout password=SensitivePassword token=SensitiveToken", ErrorCategory: "authentication", ResponseCode: 403}
	s := &Server{Store: db, Secrets: box, Sync: syncer, RuntimeStatus: func(context.Context) RuntimeStatus { return runtime }, Ready: func(context.Context) error { return errors.New("SIP registration is failed") }}
	h := s.Handler()
	csrf, cookies := issue(t, h)
	var userID string
	if err := db.DB.QueryRow(`SELECT id FROM users WHERE username='admin'`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	account := store.Account{ID: "owned", UserID: userID, CanonicalName: "Owned", Email: "owner@example.test", IMAPHost: "imap.example.test", IMAPUser: "u", IMAPPassword: "SensitivePassword", SMTPHost: "smtp.example.test", SMTPUser: "u", SMTPPassword: "SensitivePassword", FolderMap: "{}"}
	if err := db.SaveAccount(ctx, box, account); err != nil {
		t.Fatal(err)
	}
	never := account
	never.ID = "never"
	if err := db.SaveAccount(ctx, box, never); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateUser(ctx, store.User{ID: "other", Username: "other", Role: "user", PasswordHash: "p", PINHash: "p", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	foreign := account
	foreign.ID = "foreign"
	foreign.UserID = "other"
	if err := db.SaveAccount(ctx, box, foreign); err != nil {
		t.Fatal(err)
	}
	foreignRun, err := db.BeginSyncRun(ctx, foreign.ID, "manual")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishSyncRun(ctx, foreignRun, false, false, errors.New("SensitiveForeignFailure")); err != nil {
		t.Fatal(err)
	}
	previousID, err := db.BeginSyncRun(ctx, account.ID, "initial")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishSyncRun(ctx, previousID, true, true, nil); err != nil {
		t.Fatal(err)
	}
	get := func(path string, authenticated bool) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest("GET", path, nil)
		if authenticated {
			for _, c := range cookies {
				r.AddCookie(c)
			}
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for _, path := range []string{"/api/v1/status", "/api/v1/accounts"} {
		if w := get(path, false); w.Code != 401 {
			t.Fatalf("unauthenticated %s: %d", path, w.Code)
		}
	}
	status := get("/api/v1/status", true)
	var snapshot operationalSnapshot
	if err := json.Unmarshal(status.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if status.Code != 200 || snapshot.Runtime.Registration != "failed" || snapshot.Readiness != "not_ready" || snapshot.SIPFailure == nil || snapshot.SIPFailure.Diagnostic != "SIP response 403" || snapshot.CheckedAt == "" {
		t.Fatalf("incorrect SIP snapshot: %d %+v", status.Code, snapshot)
	}
	if strings.Contains(status.Body.String(), "Sensitive") || status.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("snapshot leaked or cached: %s", status.Body.String())
	}
	if public := get("/readyz", false); strings.Contains(public.Body.String(), "Sensitive") || strings.Contains(public.Body.String(), "sip_failure") {
		t.Fatal("public readiness leaked private diagnostics")
	}
	// A real synchronization service owns work while its resolver is blocked.
	runCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- syncer.RefreshAccount(runCtx, userID, account.ID) }()
	select {
	case <-entered:
	case <-runCtx.Done():
		t.Fatal("sync did not start")
	}
	decodeAccounts := func() map[string]accountView {
		t.Helper()
		w := get("/api/v1/accounts", true)
		var views []accountView
		if w.Code != 200 {
			t.Fatalf("accounts: %d %s", w.Code, w.Body.String())
		}
		if err := json.Unmarshal(w.Body.Bytes(), &views); err != nil {
			t.Fatal(err)
		}
		if len(views) != 2 || strings.Contains(w.Body.String(), "Sensitive") {
			t.Fatalf("ownership or diagnostic privacy failed: %s", w.Body.String())
		}
		out := map[string]accountView{}
		for _, v := range views {
			out[v.ID] = v
		}
		return out
	}
	views := decodeAccounts()
	if views["never"].LastSync != nil || views["owned"].LastSync.State != "running" || views["owned"].LastSuccessfulSync.ID != previousID {
		t.Fatalf("incorrect live sync: %+v", views)
	}
	close(release)
	if err := <-done; err == nil {
		t.Fatal("failing mbsync succeeded")
	}
	views = decodeAccounts()
	failed := views["owned"].LastSync
	if failed.State != "failed" || failed.FinishedAt == "" || failed.Failure == nil || failed.Failure.Category != "authentication" || failed.Failure.Diagnostic != "Child process exit status 7" || views["owned"].LastSuccessfulSync.ID != previousID {
		t.Fatalf("missing failure/recency: %+v", failed)
	}
	if syncer.SyncRunning(account.ID) {
		t.Fatal("completed sync still active")
	}
	// Manual errors must use the same safe diagnostic boundary as durable views.
	s.Sync = statusFailureSync{}
	r := httptest.NewRequest("POST", "/api/v1/accounts/owned/sync", nil)
	r.Header.Set("X-CSRF-Token", csrf)
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 502 || !strings.Contains(w.Body.String(), "rejected authentication") || strings.Contains(w.Body.String(), "Sensitive") {
		t.Fatalf("manual sync error is hidden or leaks credentials: %d %s", w.Code, w.Body.String())
	}
	s.Sync = syncer
	// Without a worker, an unfinished record must never be called 'running'.
	if _, err := db.BeginSyncRun(ctx, account.ID, "manual"); err != nil {
		t.Fatal(err)
	}
	if view := decodeAccounts()["owned"].LastSync; view.State != "unfinished" {
		t.Fatalf("abandoned record claims to run: %+v", view)
	}
	if _, err := db.InterruptSyncRuns(ctx); err != nil {
		t.Fatal(err)
	}
	if view := decodeAccounts()["owned"].LastSync; view.State != "failed" || view.Failure.Category != "interrupted" {
		t.Fatalf("missing restart result: %+v", view)
	}
	runtime.Registration = "registered"
	runtime.LastError = ""
	runtime.ErrorCategory = ""
	runtime.ResponseCode = 0
	s.Ready = nil
	s.OfflineMode = true
	status = get("/api/v1/status", true)
	if err := json.Unmarshal(status.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if !snapshot.OfflineMode || snapshot.Runtime.Registration != "registered" || strings.Contains(status.Body.String(), "sip_failure") {
		t.Fatalf("success did not clear SIP failure: %s", status.Body.String())
	}
	if _, err := db.DB.Exec(`UPDATE users SET role='user' WHERE id=?`, userID); err != nil {
		t.Fatal(err)
	}
	if ordinary := get("/api/v1/status", true); ordinary.Code != 200 {
		t.Fatalf("regular users cannot see runtime state: %d", ordinary.Code)
	}
	if restricted := get("/api/v1/sip", true); restricted.Code != 403 {
		t.Fatalf("regular users can see SIP settings: %d", restricted.Code)
	}
}

func TestStructuredSyncFailureIgnoresProviderText(t *testing.T) {
	server := &Server{}
	run := store.SyncRun{FinishedAt: "finished", ErrorCategory: string(operation.Authentication), Error: "TLS timeout password=SensitivePassword"}
	view := server.syncRunJSON(run)
	if view.Failure.Category != "authentication" || strings.Contains(view.Error, "SensitivePassword") {
		t.Fatalf("bad structured failure: %+v", view)
	}
	run.ErrorCategory = "password=SensitiveCategory"
	view = server.syncRunJSON(run)
	if view.Failure.Category != "operation_failed" {
		t.Fatalf("unknown category exposed: %+v", view)
	}
}

func TestOperationalErrorResponsePreservesCategoryAndRequestID(t *testing.T) {
	w := httptest.NewRecorder()
	w.Header().Set("X-Request-ID", "test-request")
	cause := operation.Wrap("smtp", "authenticate", operation.Authentication, errors.New("provider rejected secret credentials"))
	writeOperationalError(w, http.StatusBadGateway, cause)
	var response apiErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusBadGateway || response.Code != apiErrorCode(http.StatusBadGateway) || response.RequestID != "test-request" || response.Failure == nil || response.Failure.Category != "authentication" {
		t.Fatalf("bad response: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "secret credentials") {
		t.Fatal("provider prose leaked")
	}
}

func (p statusFailureSync) StartRefreshAccount(_ context.Context, worker context.Context, userID, accountID string) (int64, error) {
	return 0, p.RefreshAccount(worker, userID, accountID)
}
func (statusFailureSync) WaitManualSyncs() {}
