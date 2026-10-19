package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/voxmail/voxmail/internal/mailer"
	"github.com/voxmail/voxmail/internal/secret"
	"github.com/voxmail/voxmail/internal/speech"
	"github.com/voxmail/voxmail/internal/store"
)

type offlineOutboundProbe struct{ calls atomic.Int32 }

func (p *offlineOutboundProbe) Apply(context.Context) error { p.calls.Add(1); return nil }
func (p *offlineOutboundProbe) RefreshAccount(context.Context, string, string) error {
	p.calls.Add(1)
	return nil
}
func (p *offlineOutboundProbe) TestAlert(context.Context, string) error { p.calls.Add(1); return nil }
func (p *offlineOutboundProbe) Install(context.Context, string, speech.VoiceSpec) error {
	p.calls.Add(1)
	return errors.New("fixture installer")
}

func TestOfflineHTTPContractSuppressesManualProviderOperations(t *testing.T) {
	srv, h := voiceWebFixture(t)
	srv.OfflineMode = true
	box, err := secret.New("offline-contract-disposable-key-32-bytes-long")
	if err != nil {
		t.Fatal(err)
	}
	srv.Secrets = box
	account := store.Account{ID: "restored-account", UserID: "user", CanonicalName: "Restored mailbox", Email: "restored@example.test", IMAPHost: "imap.invalid", IMAPPort: 993, IMAPSecurity: "implicit_tls", IMAPUser: "fixture", IMAPPassword: "fixture-imap", SMTPHost: "smtp.invalid", SMTPPort: 465, SMTPSecurity: "implicit_tls", SMTPUser: "fixture", SMTPPassword: "fixture-smtp", FolderMap: "{}"}
	if err := srv.Store.SaveAccount(context.Background(), box, account); err != nil {
		t.Fatal(err)
	}
	probe := &offlineOutboundProbe{}
	srv.SIP = probe
	srv.Sync = probe
	srv.Alerts = probe
	srv.VoiceInstaller = probe
	srv.SendRecovery = func(context.Context, mailer.Config, []string, []byte) mailer.SendResult {
		probe.calls.Add(1)
		return mailer.SendResult{Status: mailer.SendAccepted}
	}
	for _, test := range []struct {
		role, path string
		body       any
	}{
		{"user", "/api/v1/accounts/validate", map[string]string{"imap_host": "imap.invalid", "smtp_host": "smtp.invalid"}},
		{"user", "/api/v1/accounts/test", map[string]string{"account_id": account.ID}},
		{"user", "/api/v1/accounts/" + account.ID + "/sync", nil},
		{"user", "/api/v1/alerts/test", nil},
		{"admin", "/api/v1/voices/install", map[string]string{"voice": "en_US-lessac-low"}},
		{"", "/api/v1/recovery/request", map[string]string{"email": account.Email, "purpose": "reset_password"}},
		{"", "/api/v1/recovery/request", map[string]string{"email": "unknown@example.test", "purpose": "bypass_2fa"}},
	} {
		response := voiceRequest(h, test.role, http.MethodPost, test.path, test.body)
		if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "offline mode") {
			t.Fatalf("%s: %d %s", test.path, response.Code, response.Body.String())
		}
	}
	if probe.calls.Load() != 0 {
		t.Fatal("offline operations invoked provider dependencies", probe.calls.Load())
	}
	for _, table := range []string{"sync_runs", "voice_install_jobs", "recovery_tokens"} {
		var count int
		if err := srv.Store.DB.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("offline created %s work: %d %v", table, count, err)
		}
	}
	// SIP can be edited during maintenance, but no controller is applied.
	response := voiceRequest(h, "admin", http.MethodPut, "/api/v1/sip", map[string]any{"domain": "sip.invalid", "username": "restored", "password": "fixture", "enabled": true})
	if response.Code != http.StatusOK {
		t.Fatal(response.Code, response.Body.String())
	}
	sip, err := srv.Store.GetSIP(context.Background())
	if err != nil || !sip.Enabled || sip.Domain != "sip.invalid" || probe.calls.Load() != 0 {
		t.Fatalf("offline SIP persistence/apply: %+v %v %d", sip, err, probe.calls.Load())
	}
	if response := voiceRequest(h, "user", http.MethodPut, "/api/v1/sip", map[string]any{}); response.Code != http.StatusForbidden {
		t.Fatal("offline bypassed admin role", response.Code)
	}
	if response := voiceRequest(h, "", http.MethodPost, "/api/v1/accounts/test", nil); response.Code != http.StatusUnauthorized {
		t.Fatal("offline bypassed authentication", response.Code)
	}
	if response := voiceRequest(h, "", http.MethodGet, "/readyz", nil); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"offline_mode":true`) {
		t.Fatal("mode missing from readiness", response.Code, response.Body.String())
	}
	if response := voiceRequest(h, "", http.MethodGet, "/api/v1", nil); response.Code != http.StatusOK || strings.Contains(response.Body.String(), `"offline_mode"`) {
		t.Fatal("api info leaked offline mode unauthenticated", response.Code, response.Body.String())
	}
	if response := voiceRequest(h, "user", http.MethodGet, "/api/v1", nil); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"offline_mode":true`) {
		t.Fatal("mode missing from authenticated API", response.Code, response.Body.String())
	}
	if response := voiceRequest(h, "user", http.MethodPost, "/api/v1/contacts", map[string]string{"name": "Maintenance contact", "email": "contact@example.test"}); response.Code != http.StatusCreated {
		t.Fatal("offline rejected local maintenance", response.Code, response.Body.String())
	}
}

func TestShutdownHTTPAdmissionRejectsWorkButKeepsHealth(t *testing.T) {
	srv, h := voiceWebFixture(t)
	srv.BeginShutdown()
	srv.RuntimeStatus = func(context.Context) RuntimeStatus {
		t.Error("shutdown readiness used runtime dependencies")
		return RuntimeStatus{}
	}
	for _, request := range []struct{ method, path string }{
		{http.MethodGet, "/"}, {http.MethodGet, "/api/v1/me"}, {http.MethodPost, "/api/v1/contacts"},
		{http.MethodPost, "/api/v1/setup"}, {http.MethodPost, "/api/v1/voices/install"},
	} {
		response := voiceRequest(h, "admin", request.method, request.path, map[string]string{})
		if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "shutting down") {
			t.Fatalf("shutdown admitted %s %s: %d %s", request.method, request.path, response.Code, response.Body.String())
		}
	}
	if response := voiceRequest(h, "", http.MethodGet, "/healthz", nil); response.Code != http.StatusOK {
		t.Fatal("shutdown removed process health", response.Code)
	}
	response := voiceRequest(h, "", http.MethodGet, "/readyz", nil)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"status":"not_ready"`) || !strings.Contains(response.Body.String(), "shutting down") {
		t.Fatal("shutdown claimed readiness", response.Code, response.Body.String())
	}
	var contacts int
	if err := srv.Store.DB.QueryRow("SELECT COUNT(*) FROM contacts").Scan(&contacts); err != nil || contacts != 0 {
		t.Fatal("shutdown mutated local data", contacts, err)
	}
}

// Closing HTTP connections cancels a request but does not join its goroutine.
// The application must retain the database until handler cleanup has finished.
func TestCloseJoinsCanceledHTTPHandlerFinalUpdate(t *testing.T) {
	srv, _ := voiceWebFixture(t)
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	finalUpdate := make(chan error, 1)
	httpServer := httptest.NewServer(srv.withAdmission(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(canceled)
		<-release
		finalUpdate <- srv.Store.Audit(context.Background(), "admin", "shutdown_cleanup", "request finished")
	})))
	defer httpServer.Close()
	defer unblock()
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		response, _ := (&http.Client{Timeout: time.Second}).Get(httpServer.URL)
		if response != nil {
			response.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("HTTP operation did not start")
	}
	srv.BeginShutdown()
	httpServer.CloseClientConnections()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("HTTP operation did not observe cancellation")
	}
	joined := make(chan struct{})
	go func() { srv.Close(); close(joined) }()
	select {
	case <-joined:
		t.Fatal("server joined before admitted handler finished its final update")
	case <-time.After(25 * time.Millisecond):
	}
	unblock()
	select {
	case <-joined:
	case <-time.After(time.Second):
		t.Fatal("server did not join completed handler")
	}
	if err := <-finalUpdate; err != nil {
		t.Fatal("handler lost database during cleanup", err)
	}
	var count int
	if err := srv.Store.DB.QueryRow("SELECT COUNT(*) FROM audit_log WHERE action = 'shutdown_cleanup'").Scan(&count); err != nil || count != 1 {
		t.Fatal("final request update not durable", count, err)
	}
	<-clientDone
}
