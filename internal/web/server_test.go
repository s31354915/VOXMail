package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/voxmail/voxmail/internal/auth"
	"github.com/voxmail/voxmail/internal/secret"
	"github.com/voxmail/voxmail/internal/store"
)

type fakeSIP struct {
	applied int
}

func (f *fakeSIP) Apply(context.Context) error {
	f.applied++
	return nil
}

type fakeSIPGate struct {
	applyErr   error
	beginCalls int
}

func (f *fakeSIPGate) Apply(context.Context) error { return nil }

func (f *fakeSIPGate) BeginSIPApply(context.Context) (func(), error) {
	f.beginCalls++
	if f.applyErr != nil {
		return nil, f.applyErr
	}
	return func() {}, nil
}

func (f *fakeSIPGate) InvalidateUserSessions(string) {}

type fakeAlerts struct {
	calls int
}

func (f *fakeAlerts) TestAlert(context.Context, string) error {
	f.calls++
	return nil
}

type deletionSyncProbe struct {
	db           *store.Store
	stopped      []string
	deletingSeen bool
}

func (p *deletionSyncProbe) RefreshAccount(context.Context, string, string) error { return nil }

func (p *deletionSyncProbe) StopAccount(ctx context.Context, accountID string) error {
	p.stopped = append(p.stopped, accountID)
	var deleting int
	if err := p.db.DB.QueryRowContext(ctx, `SELECT deleting FROM accounts WHERE id=?`, accountID).Scan(&deleting); err != nil {
		return err
	}
	p.deletingSeen = deleting == 1
	return nil
}

type deletionCallsProbe struct {
	db           *store.Store
	invalidated  []string
	deletingSeen bool
}

func (p *deletionCallsProbe) InvalidateUserSessions(userID string) {}

func (p *deletionCallsProbe) InvalidateUserSessionsForDeletion(userID string) {
	p.invalidated = append(p.invalidated, userID)
	var count int
	_ = p.db.DB.QueryRow(`SELECT COUNT(*) FROM accounts WHERE user_id=? AND deleting=1`, userID).Scan(&count)
	p.deletingSeen = count == 1
}

func TestDeleteAccountDataCleansOwnedArtifactsAfterCoordination(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "delete.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.CreateUser(ctx, store.User{ID: "delete-web-user", Username: "delete-web-user", PasswordHash: "p", PINHash: "p", Role: "user", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('delete-web-account','delete-web-user','Work','work@example.com','Work','imap','user','sealed','smtp','user','sealed','{}','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO drafts(id,user_id,account_id,created_at,updated_at) VALUES('delete-web-draft','delete-web-user','delete-web-account','now','now')`); err != nil {
		t.Fatal(err)
	}
	dataRoot := t.TempDir()
	maildir := filepath.Join(dataRoot, "mail", "delete-web-account")
	config := filepath.Join(dataRoot, "config", "mbsync", "delete-web-account.conf")
	quarantine := filepath.Join(dataRoot, "quarantine", "delete-web-account")
	draftDir := filepath.Join(dataRoot, "drafts", store.DraftStorageKey("delete-web-draft"))
	attachment := filepath.Join(draftDir, "generation", "0.bin")
	for _, dir := range []string{maildir, filepath.Dir(config), quarantine, filepath.Dir(attachment)} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{filepath.Join(maildir, "message"), config, filepath.Join(quarantine, "message"), attachment} {
		if err := os.WriteFile(path, []byte("private"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO draft_attachments(draft_id,filename,content_type,path,size) VALUES('delete-web-draft','recording.wav','audio/wav',?,7)`, attachment); err != nil {
		t.Fatal(err)
	}
	syncProbe := &deletionSyncProbe{db: db}
	callsProbe := &deletionCallsProbe{db: db}
	server := &Server{Store: db, DataRoot: dataRoot, Sync: syncProbe, Calls: callsProbe}
	pending, err := server.deleteAccountData(ctx, "delete-web-user", "delete-web-account", true)
	if err != nil {
		t.Fatal(err)
	}
	if pending {
		t.Fatal("owned cleanup unexpectedly remained pending")
	}
	if len(syncProbe.stopped) != 1 || syncProbe.stopped[0] != "delete-web-account" || !syncProbe.deletingSeen {
		t.Fatalf("sync coordination=%+v deleting_seen=%v", syncProbe.stopped, syncProbe.deletingSeen)
	}
	if len(callsProbe.invalidated) != 1 || callsProbe.invalidated[0] != "delete-web-user" || !callsProbe.deletingSeen {
		t.Fatalf("call coordination=%+v deleting_seen=%v", callsProbe.invalidated, callsProbe.deletingSeen)
	}
	for _, path := range []string{maildir, config, quarantine, draftDir} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("owned artifact survived at %q: %v", path, err)
		}
	}
	var accountCount int
	if err := db.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE id='delete-web-account'`).Scan(&accountCount); err != nil {
		t.Fatal(err)
	}
	if accountCount != 0 {
		t.Fatalf("account metadata survived deletion: %d", accountCount)
	}
	var status string
	if err := db.DB.QueryRowContext(ctx, `SELECT status FROM cleanup_jobs WHERE account_id='delete-web-account'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "complete" {
		t.Fatalf("cleanup job status=%q, want complete", status)
	}
}

func TestValidAccountPathIDAllowsHexadecimalZero(t *testing.T) {
	for _, test := range []struct {
		name string
		id   string
		want bool
	}{
		{name: "ordinary hex id", id: "d836235ce3cee64d4c524a0b660ab2de", want: true},
		{name: "slash", id: "account/id", want: false},
		{name: "backslash", id: `account\\id`, want: false},
		{name: "nul", id: "account\x00id", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := validAccountPathID(test.id); got != test.want {
				t.Fatalf("validAccountPathID(%q)=%v, want %v", test.id, got, test.want)
			}
		})
	}
}

func TestNormalizeAlertFoldersUsesLocalInboxByDefault(t *testing.T) {
	if got := normalizeAlertFolders(nil, map[string]string{"INBOX": "Inbox"}); len(got) != 1 || got[0] != "Inbox" {
		t.Fatalf("default mapped folders=%v", got)
	}
	got := normalizeAlertFolders([]string{"INBOX", "Inbox", "Junk"}, map[string]string{"INBOX": "Inbox", "Junk": "Spam"})
	if len(got) != 2 || got[0] != "Inbox" || got[1] != "Spam" {
		t.Fatalf("normalized folders=%v", got)
	}
}

func TestSecurityHeadersAllowOnlyGeneratedPreviewMedia(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	box, _ := secret.New("test-key-with-more-than-32-characters-123456")
	h := (&Server{Store: db, Secrets: box}).Handler()
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Header().Get("X-Request-ID") == "" {
		t.Fatal("health response missing request ID")
	}
	policy := recorder.Header().Get("Content-Security-Policy")
	if !strings.Contains(policy, "media-src 'self' blob:") || strings.Contains(policy, "unsafe-inline") {
		t.Fatalf("unexpected media policy: %q", policy)
	}
	apiRecorder := httptest.NewRecorder()
	h.ServeHTTP(apiRecorder, httptest.NewRequest(http.MethodGet, "/api/v1", nil))
	if apiRecorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("API response cache policy=%q, want no-store", apiRecorder.Header().Get("Cache-Control"))
	}
}

func TestContentSecurityPolicyHashesEmbeddedAssets(t *testing.T) {
	html := string(indexHTML)
	assetHash := func(open, close string) string {
		start := strings.Index(html, open)
		if start < 0 {
			t.Fatalf("missing embedded asset %q", open)
		}
		tagEnd := strings.Index(html[start:], ">")
		if tagEnd < 0 {
			t.Fatalf("unclosed embedded asset tag %q", open)
		}
		start += tagEnd + 1
		end := strings.Index(html[start:], close)
		if end < 0 {
			t.Fatalf("unclosed embedded asset %q", open)
		}
		digest := sha256.Sum256([]byte(html[start : start+end]))
		return base64.StdEncoding.EncodeToString(digest[:])
	}
	scriptHash := assetHash("<script", "</script>")
	styleHash := assetHash("<style", "</style>")
	recorder := httptest.NewRecorder()
	withSecurityHeaders(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	policy := recorder.Header().Get("Content-Security-Policy")
	for _, required := range []string{"'sha256-" + scriptHash + "'", "'sha256-" + styleHash + "'"} {
		if !strings.Contains(policy, required) {
			t.Fatalf("CSP is missing embedded asset hash %q: %s", required, policy)
		}
	}
}

func TestAPIErrorHasStableCodeAndRequestID(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/errors.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h := (&Server{Store: db}).Handler()
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/me", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", recorder.Code)
	}
	var response apiErrorResponse
	if err := json.NewDecoder(recorder.Result().Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Error == "" || response.Code != "unauthorized" || response.RequestID == "" || response.RequestID != recorder.Header().Get("X-Request-ID") {
		t.Fatalf("error response=%+v headers=%v", response, recorder.Header())
	}
}

func TestVoiceSaveControlIsOutsideAlertOnlyContainer(t *testing.T) {
	html := string(indexHTML)
	alertStart := strings.Index(html, `<div id="alert-controls">`)
	if alertStart < 0 {
		t.Fatal("alert-only settings container is missing")
	}
	alertClose := strings.Index(html[alertStart:], "</div>")
	if alertClose < 0 {
		t.Fatal("alert-only settings container is not closed")
	}
	alertClose += alertStart
	save := strings.Index(html, "Save voice settings")
	if save < 0 {
		t.Fatal("voice save control is missing")
	}
	if save > alertStart && save < alertClose {
		t.Fatal("voice save control is inside the alert-only container")
	}
	formStart := strings.Index(html, `<form id="settings-form">`)
	formClose := strings.Index(html[formStart:], "</form>")
	if formStart < 0 || formClose < 0 || save < formStart || save > formStart+formClose {
		t.Fatal("voice save control is not a keyboard-submit control in settings form")
	}
}

func TestBrowserFormsDeclareExplicitSubmitControls(t *testing.T) {
	html := string(indexHTML)
	for _, id := range []string{
		"setup", "login", "recovery-request", "recovery-confirm", "account-form",
		"contact-form", "phone-form", "settings-form", "alert-number-form",
		"user-form", "sip-form", "password-form", "pin-form",
	} {
		start := strings.Index(html, `<form id="`+id+`"`)
		if start < 0 {
			t.Fatalf("form %q is missing", id)
		}
		end := strings.Index(html[start:], "</form>")
		if end < 0 {
			t.Fatalf("form %q is not closed", id)
		}
		markup := html[start : start+end]
		if !strings.Contains(markup, `<button type="submit"`) {
			t.Fatalf("form %q does not declare an explicit submit button", id)
		}
		if (id == "setup" || id == "login") && !strings.Contains(markup, `method="post"`) {
			t.Fatalf("auth form %q must use POST as a safe no-JavaScript fallback", id)
		}
	}
}

func TestLoginKeysIgnoreEphemeralPort(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/login", nil)
	r.RemoteAddr = "203.0.113.10:41000"
	first := loginKeys(r, "Admin")
	r.RemoteAddr = "203.0.113.10:52000"
	second := loginKeys(r, "Admin")
	if strings.Join(first, "\x00") != strings.Join(second, "\x00") {
		t.Fatalf("login keys changed with source port: %v vs %v", first, second)
	}
}

func TestDecodeRejectsTrailingJSON(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h := (&Server{Store: db}).Handler()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/setup", bytes.NewBufferString(`{"username":"admin","password":"a-strong-password","pin":"1234"}{}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("trailing JSON status=%d, want 400", w.Code)
	}
}

func TestBootstrapSetupHTTPHasOneWinner(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "setup-http.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h := (&Server{Store: db, Sessions: NewSessionStore()}).Handler()

	const attempts = 8
	start := make(chan struct{})
	type result struct {
		status  int
		cookies int
	}
	results := make(chan result, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			body := fmt.Sprintf(`{"username":"admin-%d","password":"bootstrap-password-%02d","pin":"%04d"}`, i, i, i)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/setup", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			results <- result{status: w.Code, cookies: len(w.Result().Cookies())}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	statusCounts := map[int]int{}
	winnerCookies := 0
	for got := range results {
		statusCounts[got.status]++
		if got.status == http.StatusCreated {
			winnerCookies = got.cookies
		}
	}
	if statusCounts[http.StatusCreated] != 1 || statusCounts[http.StatusConflict] != attempts-1 {
		t.Fatalf("setup statuses=%v, want one 201 and %d 409s", statusCounts, attempts-1)
	}
	if winnerCookies != 1 {
		t.Fatalf("winning setup response cookies=%d, want one session cookie", winnerCookies)
	}

	var users, settings, setupCompleted int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users); err != nil {
		t.Fatal(err)
	}
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM settings`).Scan(&settings); err != nil {
		t.Fatal(err)
	}
	if err := db.DB.QueryRow(`SELECT setup_completed FROM system_settings WHERE id=1`).Scan(&setupCompleted); err != nil {
		t.Fatal(err)
	}
	if users != 1 || settings != 1 || setupCompleted != 1 {
		t.Fatalf("bootstrap state users=%d settings=%d setup_completed=%d", users, settings, setupCompleted)
	}

	// A fresh handler must still reject setup, even after the user row is
	// removed as part of an administrative repair. The durable marker, not a
	// transient users-table count, owns the one-time boundary.
	if _, err := db.DB.Exec(`DELETE FROM users`); err != nil {
		t.Fatal(err)
	}
	restarted := (&Server{Store: db, Sessions: NewSessionStore()}).Handler()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/setup", strings.NewReader(`{"username":"reopened","password":"bootstrap-password","pin":"1234"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	restarted.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("setup after durable completion status=%d, want %d", w.Code, http.StatusConflict)
	}
}

func TestTestAlertEndpoint(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	box, _ := secret.New("test-key-with-more-than-32-characters-123456")
	alerts := &fakeAlerts{}

	svc := &Server{Store: db, Secrets: box, Alerts: alerts}
	h := svc.Handler()
	csrf, cookies := issue(t, h)

	request := httptest.NewRequest(http.MethodPost, "/api/v1/alerts/test", nil)
	request.Header.Set("X-CSRF-Token", csrf)
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	if response := recorder.Result(); response.StatusCode != http.StatusOK {
		t.Fatalf("test alert status %d", response.StatusCode)
	}
	if alerts.calls != 1 {
		t.Fatalf("alert service invoked %d times, want 1", alerts.calls)
	}
	request = httptest.NewRequest(http.MethodPut, "/api/v1/admin/alerts", bytes.NewBufferString(`{"available":false}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", csrf)
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	recorder = httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	if response := recorder.Result(); response.StatusCode != http.StatusOK {
		t.Fatalf("global alert disable status %d", response.StatusCode)
	}
	request = httptest.NewRequest(http.MethodPost, "/api/v1/alerts/test", nil)
	request.Header.Set("X-CSRF-Token", csrf)
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	recorder = httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	if response := recorder.Result(); response.StatusCode != http.StatusNotFound {
		t.Fatalf("disabled alert endpoint status %d, want 404", response.StatusCode)
	}
	var adminID string
	if err := db.DB.QueryRowContext(context.Background(), `SELECT id FROM users WHERE username='admin'`).Scan(&adminID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(context.Background(), `UPDATE settings SET alerts_enabled=1,alert_phone='+15550001111' WHERE user_id=?`, adminID); err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodPut, "/api/v1/settings", bytes.NewBufferString(`{"tts_voice":"en_US-hfc_male-medium","menu_speed":4,"email_speed":5}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", csrf)
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	recorder = httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	if response := recorder.Result(); response.StatusCode != http.StatusOK {
		t.Fatalf("admin voice settings save while alerts disabled status %d", response.StatusCode)
	}
	var adminAlerts int
	var adminPhone string
	if err := db.DB.QueryRowContext(context.Background(), `SELECT alerts_enabled,COALESCE(alert_phone,'') FROM settings WHERE user_id=?`, adminID).Scan(&adminAlerts, &adminPhone); err != nil {
		t.Fatal(err)
	}
	if adminAlerts != 1 || adminPhone != "+15550001111" {
		t.Fatalf("admin voice save erased disabled alert settings: enabled=%d phone=%q", adminAlerts, adminPhone)
	}
	request = httptest.NewRequest(http.MethodPost, "/api/v1/users", bytes.NewBufferString(`{"username":"ordinary","password":"another-strong-password","pin":"5678"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", csrf)
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	recorder = httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	if response := recorder.Result(); response.StatusCode != http.StatusCreated {
		t.Fatalf("ordinary user creation status %d", response.StatusCode)
	}

	request = httptest.NewRequest(http.MethodPost, "/api/v1/login", bytes.NewBufferString(`{"username":"ordinary","password":"another-strong-password"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder = httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	loginResponse := recorder.Result()
	if loginResponse.StatusCode != http.StatusOK {
		t.Fatalf("ordinary user login status %d", loginResponse.StatusCode)
	}
	userCSRF := loginResponse.Header.Get("X-CSRF-Token")
	userCookies := loginResponse.Cookies()
	if _, err := db.DB.ExecContext(context.Background(), `UPDATE settings SET alerts_enabled=1,alert_phone='+15550009999' WHERE user_id=(SELECT id FROM users WHERE username='ordinary')`); err != nil {
		t.Fatal(err)
	}
	var ordinaryID string
	if err := db.DB.QueryRowContext(context.Background(), `SELECT id FROM users WHERE username='ordinary'`).Scan(&ordinaryID); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveAlertNumber(context.Background(), ordinaryID, "+15550009999", true); err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
	for _, cookie := range userCookies {
		request.AddCookie(cookie)
	}
	recorder = httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	if response := recorder.Result(); response.StatusCode != http.StatusOK {
		t.Fatalf("ordinary user settings status %d", response.StatusCode)
	} else {
		var settings map[string]any
		if err := json.NewDecoder(response.Body).Decode(&settings); err != nil {
			t.Fatal(err)
		}
		if _, exists := settings["alerts_enabled"]; exists {
			t.Fatal("disabled alert controls leaked alerts_enabled to ordinary user")
		}
		if _, exists := settings["alert_phone"]; exists {
			t.Fatal("disabled alert controls leaked alert_phone to ordinary user")
		}
	}
	request = httptest.NewRequest(http.MethodGet, "/api/v1/alert-numbers", nil)
	for _, cookie := range userCookies {
		request.AddCookie(cookie)
	}
	recorder = httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	if response := recorder.Result(); response.StatusCode != http.StatusNotFound {
		t.Fatalf("ordinary user alert-number status %d, want 404", response.StatusCode)
	}
	request = httptest.NewRequest(http.MethodGet, "/api/v1/voices", nil)
	for _, cookie := range userCookies {
		request.AddCookie(cookie)
	}
	recorder = httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	if response := recorder.Result(); response.StatusCode != http.StatusOK {
		t.Fatalf("ordinary user voices status %d", response.StatusCode)
	}
	request = httptest.NewRequest(http.MethodPost, "/api/v1/voices/install", bytes.NewBufferString(`{"voice":"en_US-hfc_male-medium"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", userCSRF)
	for _, cookie := range userCookies {
		request.AddCookie(cookie)
	}
	recorder = httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	if response := recorder.Result(); response.StatusCode != http.StatusForbidden {
		t.Fatalf("ordinary user voice install status %d, want 403", response.StatusCode)
	}
	request = httptest.NewRequest(http.MethodPut, "/api/v1/settings", bytes.NewBufferString(`{"tts_voice":"en_US-hfc_male-medium","alerts_enabled":true,"alert_phone":"+15551234567"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", userCSRF)
	for _, cookie := range userCookies {
		request.AddCookie(cookie)
	}
	recorder = httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	if response := recorder.Result(); response.StatusCode != http.StatusOK {
		t.Fatalf("ordinary user settings save status %d", response.StatusCode)
	}
	var saved map[string]any
	if err := json.NewDecoder(recorder.Result().Body).Decode(&saved); err != nil {
		t.Fatal(err)
	}
	if enabled, _ := saved["alerts_enabled"].(bool); enabled {
		t.Fatal("ordinary user could enable globally disabled alerts")
	}
	if _, exists := saved["alert_phone"]; exists {
		t.Fatal("ordinary user response exposed alert_phone while alerts were disabled")
	}
	var enabled int
	var phone string
	if err := db.DB.QueryRowContext(context.Background(), `SELECT alerts_enabled,COALESCE(alert_phone,'') FROM settings WHERE user_id=(SELECT id FROM users WHERE username='ordinary')`).Scan(&enabled, &phone); err != nil {
		t.Fatal(err)
	}
	if enabled != 1 || phone != "+15550009999" {
		t.Fatalf("disabled global alerts erased saved preferences: enabled=%d phone=%q", enabled, phone)
	}
	request = httptest.NewRequest(http.MethodPut, "/api/v1/admin/alerts", bytes.NewBufferString(`{"available":true}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", csrf)
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	recorder = httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	if response := recorder.Result(); response.StatusCode != http.StatusOK {
		t.Fatalf("global alert enable status %d", response.StatusCode)
	}

	svc.Alerts = nil
	request = httptest.NewRequest(http.MethodPost, "/api/v1/alerts/test", nil)
	request.Header.Set("X-CSRF-Token", csrf)
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	recorder = httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	if response := recorder.Result(); response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unwired service status %d, want 503", response.StatusCode)
	}
}

func TestSetupAndCSRFProtectedContact(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	box, _ := secret.New("test-key-with-more-than-32-characters-123456")
	h := (&Server{Store: db, Secrets: box}).Handler()
	body := bytes.NewBufferString(`{"username":"admin","password":"a-strong-password","pin":"1234"}`)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/setup", body)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	response := recorder.Result()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("setup status %d", response.StatusCode)
	}
	csrf := response.Header.Get("X-CSRF-Token")
	if csrf == "" {
		t.Fatal("missing csrf token")
	}
	request = httptest.NewRequest(http.MethodGet, "/api/v1/contacts", nil)
	for _, cookie := range response.Cookies() {
		request.AddCookie(cookie)
	}
	listRecorder := httptest.NewRecorder()
	h.ServeHTTP(listRecorder, request)
	if listRecorder.Code != http.StatusOK {
		t.Fatalf("empty contact list status %d", listRecorder.Code)
	}
	if got := strings.TrimSpace(listRecorder.Body.String()); got != "[]" {
		t.Fatalf("empty contact list JSON=%q, want []", got)
	}
	payload, _ := json.Marshal(map[string]string{"name": "Ada", "email": "ada@example.com"})
	request = httptest.NewRequest(http.MethodPost, "/api/v1/contacts", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", csrf)
	for _, cookie := range response.Cookies() {
		request.AddCookie(cookie)
	}
	recorder = httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	response = recorder.Result()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("contact status %d", response.StatusCode)
	}
}

func TestSaveSIPRejectsActiveCallBeforePersisting(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	box, _ := secret.New("test-key-with-more-than-32-characters-123456")
	gate := &fakeSIPGate{applyErr: errors.New("active calls are in progress")}
	h := (&Server{Store: db, Secrets: box, SIP: gate, Calls: gate}).Handler()
	csrf, cookies := issue(t, h)
	body := bytes.NewBufferString(`{"domain":"sip.example.com","username":"alice","password":"secret","port":5060,"transport":"udp","reg_interval":300,"enabled":true}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/sip", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	h.ServeHTTP(response, req)
	if response.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var count int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM sip_settings`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 || gate.beginCalls != 1 {
		t.Fatalf("persisted SIP settings=%d gate calls=%d", count, gate.beginCalls)
	}
}

func TestReadyIncludesSeparateRuntimeStates(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "ready.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h := (&Server{Store: db, RuntimeStatus: func(context.Context) RuntimeStatus {
		return RuntimeStatus{Process: "running", Bridge: "connected", Registration: "unknown", Media: "ready"}
	}}).Handler()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	response := httptest.NewRecorder()
	h.ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("ready status=%d body=%s", response.Code, response.Body.String())
	}
	var payload map[string]any
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"process": "running", "bridge": "connected", "registration": "unknown", "media": "ready"} {
		if payload[key] != want {
			t.Errorf("ready[%q]=%v, want %q", key, payload[key], want)
		}
	}
}

func TestJSONTagsLoginAccountAndSIP(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	box, _ := secret.New("test-key-with-more-than-32-characters-123456")
	fsip := &fakeSIP{}
	h := (&Server{Store: db, Secrets: box, SIP: fsip}).Handler()
	csrf, cookies := issue(t, h)

	account := map[string]any{
		"canonical_name":        "Work",
		"email":                 "ada@example.com",
		"sender_name":           "Ada",
		"imap_host":             "imap.example.com",
		"imap_port":             993,
		"imap_user":             "ada",
		"imap_password":         "imap-secret",
		"smtp_host":             "smtp.example.com",
		"smtp_port":             465,
		"smtp_user":             "ada",
		"smtp_password":         "smtp-secret",
		"sync_interval_minutes": 5,
		"folder_map":            map[string]string{"INBOX": "inbox"},
		"alert_folders":         []string{"INBOX"},
	}
	payload, _ := json.Marshal(account)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/accounts", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", csrf)
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	if response := recorder.Result(); response.StatusCode != http.StatusCreated {
		data := make([]byte, 256)
		n, _ := response.Body.Read(data)
		t.Fatalf("account status %d: %s", response.StatusCode, string(data[:n]))
	}
	request = httptest.NewRequest(http.MethodGet, "/api/v1/accounts", nil)
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	recorder = httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("accounts get status %d", recorder.Code)
	}
	var saved []accountView
	if err := json.NewDecoder(recorder.Body).Decode(&saved); err != nil || len(saved) != 1 {
		t.Fatalf("saved accounts=%+v err=%v", saved, err)
	}
	account["id"] = saved[0].ID
	account["config_version"] = saved[0].ConfigVersion
	account["canonical_name"] = "Updated"
	payload, _ = json.Marshal(account)
	request = httptest.NewRequest(http.MethodPost, "/api/v1/accounts", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", csrf)
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	recorder = httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("versioned account update status %d", recorder.Code)
	}
	payload, _ = json.Marshal(account)
	request = httptest.NewRequest(http.MethodPost, "/api/v1/accounts", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", csrf)
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	recorder = httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("stale account update status %d, want %d", recorder.Code, http.StatusConflict)
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v1/sip", nil)
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	recorder = httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	if response := recorder.Result(); response.StatusCode != http.StatusOK {
		t.Fatalf("sip get status %d", response.StatusCode)
	}

	sip := map[string]any{"domain": "sip.example.com", "username": "ada", "password": "hunter2", "local_port": 5080, "registrar_port": 5070, "transport": "udp", "reg_interval": 300, "enabled": true}
	payload, _ = json.Marshal(sip)
	request = httptest.NewRequest(http.MethodPut, "/api/v1/sip", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", csrf)
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	recorder = httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	if response := recorder.Result(); response.StatusCode != http.StatusOK {
		data := make([]byte, 256)
		n, _ := response.Body.Read(data)
		t.Fatalf("sip put status %d: %s", response.StatusCode, string(data[:n]))
	}
	if fsip.applied == 0 {
		t.Fatal("fake SIP controller was not invoked")
	}

	st, err := db.GetSIP(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Domain != "sip.example.com" || !st.Enabled || st.Password == "" || st.LocalPort != 5080 || st.RegistrarPort != 5070 {
		t.Fatalf("sip settings not round-tripped: %+v", st)
	}
}

func issue(t *testing.T, h http.Handler) (string, []*http.Cookie) {
	t.Helper()
	body := bytes.NewBufferString(`{"username":"admin","password":"a-strong-password","pin":"1234"}`)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/setup", body)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	response := recorder.Result()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("setup status %d", response.StatusCode)
	}
	if csrf := response.Header.Get("X-CSRF-Token"); csrf == "" {
		t.Fatal("missing csrf token")
	}
	return response.Header.Get("X-CSRF-Token"), response.Cookies()
}

func TestMeReturnsCSRFForRestoredSession(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := &Server{Store: db}
	h := svc.Handler()
	wantCSRF, cookies := issue(t, h)

	request := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("me status=%d, want 200", recorder.Code)
	}
	if got := recorder.Header().Get("X-CSRF-Token"); got != wantCSRF {
		t.Fatalf("restored csrf=%q, want %q", got, wantCSRF)
	}
	var user map[string]any
	if err := json.NewDecoder(recorder.Body).Decode(&user); err != nil {
		t.Fatal(err)
	}
	if user["username"] != "admin" {
		t.Fatalf("restored user=%v", user)
	}
	change := httptest.NewRequest(http.MethodPost, "/api/v1/security/password", bytes.NewBufferString(`{"current_password":"a-strong-password","new_password":"a-new-strong-password"}`))
	change.Header.Set("Content-Type", "application/json")
	change.Header.Set("X-CSRF-Token", wantCSRF)
	for _, cookie := range cookies {
		change.AddCookie(cookie)
	}
	changeRecorder := httptest.NewRecorder()
	h.ServeHTTP(changeRecorder, change)
	if changeRecorder.Code != http.StatusOK {
		t.Fatalf("password change status=%d, want 200", changeRecorder.Code)
	}
	recorder = httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("password-revoked session /me status=%d, want 401", recorder.Code)
	}
	newCookies := changeRecorder.Result().Cookies()
	if len(newCookies) == 0 {
		t.Fatal("password change did not issue a replacement session")
	}
	request = httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	for _, cookie := range newCookies {
		request.AddCookie(cookie)
	}
	recorder = httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("replacement session /me status=%d, want 200", recorder.Code)
	}
}

func TestBrowserSessionContractHandlesExpiryAndSecrets(t *testing.T) {
	html := string(indexHTML)
	for _, required := range []string{"api('/api/v1/me')", "r.status===401", "showSignedOut", "clearSensitiveFields", "Your session expired; please sign in again."} {
		if !strings.Contains(html, required) {
			t.Fatalf("browser session contract missing %q", required)
		}
	}
}

func TestBrowserAccountTestingContractSeparatesUnsavedAndSavedValues(t *testing.T) {
	html := string(indexHTML)
	for _, required := range []string{
		"/api/v1/accounts/validate",
		"/api/v1/accounts/test",
		"Current unsaved values authenticated",
		"Saved account authenticated",
		"Discovered remote folders",
		"accountActionBusy",
		"finally{endAccountAction()}",
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("browser account-test contract missing %q", required)
		}
	}
}

func newRecoveryFixture(t *testing.T, enabled, withAccount bool) (*Server, *store.Store, string) {
	t.Helper()
	db, err := store.Open(t.TempDir() + "/recovery.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	passwordHash, err := auth.Hash("old-recovery-password")
	if err != nil {
		t.Fatal(err)
	}
	pinHash, err := auth.Hash("1234")
	if err != nil {
		t.Fatal(err)
	}
	userID := "recovery-user"
	if err := db.CreateUser(context.Background(), store.User{
		ID: userID, Username: "recovery", PasswordHash: passwordHash, PINHash: pinHash,
		Role: "user", Enabled: enabled,
	}); err != nil {
		t.Fatal(err)
	}
	if withAccount {
		box, err := secret.New("test-key-with-more-than-32-characters-123456")
		if err != nil {
			t.Fatal(err)
		}
		if err := db.SaveAccount(context.Background(), box, store.Account{
			ID: "recovery-account", UserID: userID, CanonicalName: "Recovery",
			Email: "recovery@example.com", SenderName: "Recovery", IMAPHost: "imap.example.com",
			IMAPUser: "recovery", IMAPPort: 993, IMAPSecurity: "implicit_tls",
			SMTPHost: "smtp.example.com", SMTPUser: "recovery", SMTPPort: 465,
			SMTPSecurity: "implicit_tls", IMAPPassword: "imap-secret", SMTPPassword: "smtp-secret",
			FolderMap: `{}`, AlertFolders: `[]`, SyncIntervalMinutes: 5,
			ReconciliationIntervalMinutes: 1440,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return &Server{Store: db}, db, userID
}

func insertRecoveryToken(t *testing.T, db *store.Store, userID, email, purpose, code string, expires time.Time) int64 {
	t.Helper()
	hash, err := auth.Hash(code)
	if err != nil {
		t.Fatal(err)
	}
	result, err := db.DB.ExecContext(context.Background(), `INSERT INTO recovery_tokens(user_id,email,token_hash,purpose,expires_at,created_at) VALUES(?,?,?,?,?,?)`, userID, email, hash, purpose, expires.UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func postRecoveryConfirm(t *testing.T, h http.Handler, email, purpose, code, newPassword string) int {
	t.Helper()
	payload, err := json.Marshal(recoveryConfirmRequest{Email: email, Purpose: purpose, Code: code, NewPassword: newPassword})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/recovery/confirm", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	return recorder.Code
}

func TestRecoveryDeliveryFailureInvalidatesTokensAndHonorsQuota(t *testing.T) {
	svc, db, userID := newRecoveryFixture(t, true, true)
	h := svc.Handler()
	unknown := httptest.NewRequest(http.MethodPost, "/api/v1/recovery/request", bytes.NewBufferString(`{"email":"unknown@example.com","purpose":"reset_password"}`))
	unknown.RemoteAddr = "198.51.100.10:40000"
	unknown.Header.Set("Content-Type", "application/json")
	unknownRecorder := httptest.NewRecorder()
	h.ServeHTTP(unknownRecorder, unknown)
	known := httptest.NewRequest(http.MethodPost, "/api/v1/recovery/request", bytes.NewBufferString(`{"email":"recovery@example.com","purpose":"reset_password"}`))
	known.RemoteAddr = "198.51.100.10:40000"
	known.Header.Set("Content-Type", "application/json")
	knownRecorder := httptest.NewRecorder()
	h.ServeHTTP(knownRecorder, known)
	if unknownRecorder.Code != http.StatusAccepted || knownRecorder.Code != http.StatusAccepted || unknownRecorder.Body.String() != knownRecorder.Body.String() {
		t.Fatalf("recovery enumeration response mismatch: unknown=%d/%q known=%d/%q", unknownRecorder.Code, unknownRecorder.Body.String(), knownRecorder.Code, knownRecorder.Body.String())
	}
	for i := 0; i < recoveryTargetLimit+1; i++ {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/recovery/request", bytes.NewBufferString(fmt.Sprintf(`{"email":"Recovery %d <recovery@example.com>","purpose":"reset_password"}`, i)))
		request.RemoteAddr = "198.51.100.10:40000"
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		h.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusAccepted {
			t.Fatalf("request %d status=%d, want 202", i, recorder.Code)
		}
	}
	var total, active, pending int
	if err := db.DB.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM recovery_tokens WHERE user_id=?`, userID).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if err := db.DB.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM recovery_tokens WHERE user_id=? AND used_at IS NULL`, userID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if err := db.DB.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM recovery_tokens WHERE user_id=? AND token_hash LIKE 'pending:%'`, userID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if total != recoveryTargetLimit || active != 0 || pending != 0 {
		t.Fatalf("recovery rows total=%d active=%d pending=%d, want %d/0/0", total, active, pending, recoveryTargetLimit)
	}
}

func TestRecoveryRequestConcurrentQuotaCannotBeBypassed(t *testing.T) {
	svc, db, userID := newRecoveryFixture(t, true, true)
	h := svc.Handler()
	type recoveryResponse struct {
		status int
		body   string
	}
	statuses := make(chan recoveryResponse, recoveryTargetLimit+3)
	var group sync.WaitGroup
	for i := 0; i < recoveryTargetLimit+3; i++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			request := httptest.NewRequest(http.MethodPost, "/api/v1/recovery/request", bytes.NewBufferString(fmt.Sprintf(`{"email":"Attempt %d <recovery@example.com>","purpose":"reset_password"}`, index)))
			request.RemoteAddr = fmt.Sprintf("198.51.100.20:%d", 41000+index)
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			h.ServeHTTP(recorder, request)
			statuses <- recoveryResponse{status: recorder.Code, body: recorder.Body.String()}
		}(i)
	}
	group.Wait()
	close(statuses)
	for response := range statuses {
		if response.status != http.StatusAccepted {
			t.Fatalf("concurrent request status=%d body=%q, want 202", response.status, response.body)
		}
	}
	var total, active int
	if err := db.DB.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM recovery_tokens WHERE user_id=?`, userID).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if err := db.DB.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM recovery_tokens WHERE user_id=? AND used_at IS NULL`, userID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if total != recoveryTargetLimit || active != 0 {
		t.Fatalf("concurrent quota rows total=%d active=%d, want %d/0", total, active, recoveryTargetLimit)
	}
}

func TestRecoveryConfirmIsSingleUseAndExpiresTokens(t *testing.T) {
	svc, db, userID := newRecoveryFixture(t, true, false)
	h := svc.Handler()
	validID := insertRecoveryToken(t, db, userID, "recovery@example.com", "reset_password", "123456", time.Now().Add(10*time.Minute))
	if status := postRecoveryConfirm(t, h, "recovery@example.com", "reset_password", "123456", "new-recovery-password"); status != http.StatusOK {
		t.Fatalf("valid recovery status=%d, want 200", status)
	}
	if status := postRecoveryConfirm(t, h, "recovery@example.com", "reset_password", "123456", "new-recovery-password"); status != http.StatusUnauthorized {
		t.Fatalf("reused recovery status=%d, want 401", status)
	}
	var usedAt string
	if err := db.DB.QueryRowContext(context.Background(), `SELECT COALESCE(used_at,'') FROM recovery_tokens WHERE id=?`, validID).Scan(&usedAt); err != nil {
		t.Fatal(err)
	}
	if usedAt == "" {
		t.Fatal("successful recovery token remained unused")
	}
	var passwordHash string
	if err := db.DB.QueryRowContext(context.Background(), `SELECT password_hash FROM users WHERE id=?`, userID).Scan(&passwordHash); err != nil {
		t.Fatal(err)
	}
	if !auth.Check(passwordHash, "new-recovery-password") {
		t.Fatal("password recovery did not update the password")
	}

	expiredID := insertRecoveryToken(t, db, userID, "recovery@example.com", "bypass_2fa", "654321", time.Now().Add(-time.Minute))
	if status := postRecoveryConfirm(t, h, "recovery@example.com", "bypass_2fa", "654321", ""); status != http.StatusUnauthorized {
		t.Fatalf("expired recovery status=%d, want 401", status)
	}
	if err := db.DB.QueryRowContext(context.Background(), `SELECT COALESCE(used_at,'') FROM recovery_tokens WHERE id=?`, expiredID).Scan(&usedAt); err != nil {
		t.Fatal(err)
	}
	if usedAt == "" {
		t.Fatal("expired recovery token was not retired")
	}
}

func TestRecoveryConfirmRejectsDisabledUsers(t *testing.T) {
	svc, db, userID := newRecoveryFixture(t, false, false)
	h := svc.Handler()
	tokenID := insertRecoveryToken(t, db, userID, "recovery@example.com", "bypass_2fa", "123456", time.Now().Add(10*time.Minute))
	request := httptest.NewRequest(http.MethodPost, "/api/v1/recovery/confirm", bytes.NewBufferString(`{"email":"recovery@example.com","purpose":"bypass_2fa","code":"123456"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("disabled recovery status=%d, want 401", recorder.Code)
	}
	if len(recorder.Result().Cookies()) != 0 {
		t.Fatal("disabled user received a recovery session cookie")
	}
	var usedAt string
	if err := db.DB.QueryRowContext(context.Background(), `SELECT COALESCE(used_at,'') FROM recovery_tokens WHERE id=?`, tokenID).Scan(&usedAt); err != nil {
		t.Fatal(err)
	}
	if usedAt == "" {
		t.Fatal("disabled user's recovery token was not retired")
	}
}

func TestRecoveryConfirmConcurrentUseAllowsOnlyOneWinner(t *testing.T) {
	svc, db, userID := newRecoveryFixture(t, true, false)
	h := svc.Handler()
	insertRecoveryToken(t, db, userID, "recovery@example.com", "bypass_2fa", "123456", time.Now().Add(10*time.Minute))
	statuses := make(chan int, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			payload := bytes.NewBufferString(`{"email":"recovery@example.com","purpose":"bypass_2fa","code":"123456"}`)
			request := httptest.NewRequest(http.MethodPost, "/api/v1/recovery/confirm", payload)
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			h.ServeHTTP(recorder, request)
			statuses <- recorder.Code
		}()
	}
	group.Wait()
	close(statuses)
	winners := 0
	losers := 0
	for status := range statuses {
		switch status {
		case http.StatusOK:
			winners++
		case http.StatusUnauthorized:
			losers++
		default:
			t.Fatalf("concurrent recovery status=%d, want 200 or 401", status)
		}
	}
	if winners != 1 || losers != 1 {
		t.Fatalf("concurrent recovery winners=%d losers=%d, want 1/1", winners, losers)
	}
}

func TestTrustedProxyControlsForwardedIdentityAndCookieSecurity(t *testing.T) {
	svc := &Server{TrustedProxyCIDRs: []string{"203.0.113.0/24"}, Sessions: NewSessionStore()}
	direct := httptest.NewRequest(http.MethodGet, "/", nil)
	direct.RemoteAddr = "198.51.100.10:41000"
	direct.Header.Set("X-Forwarded-For", "10.0.0.7")
	direct.Header.Set("X-Forwarded-Proto", "https")
	if got := svc.clientHost(direct); got != "198.51.100.10" {
		t.Fatalf("untrusted forwarded client=%q, want direct peer", got)
	}
	directRecorder := httptest.NewRecorder()
	svc.issueSession(directRecorder, direct, "user")
	if cookies := directRecorder.Result().Cookies(); len(cookies) != 1 || cookies[0].Secure {
		t.Fatalf("untrusted forwarded proto changed cookie security: %+v", cookies)
	}

	proxied := httptest.NewRequest(http.MethodGet, "/", nil)
	proxied.RemoteAddr = "203.0.113.10:41000"
	proxied.Header.Set("X-Forwarded-For", "198.51.100.7, 203.0.113.11")
	proxied.Header.Set("X-Forwarded-Proto", "https")
	if got := svc.clientHost(proxied); got != "198.51.100.7" {
		t.Fatalf("trusted forwarded client=%q, want original client", got)
	}
	proxiedRecorder := httptest.NewRecorder()
	svc.issueSession(proxiedRecorder, proxied, "user")
	if cookies := proxiedRecorder.Result().Cookies(); len(cookies) != 1 || !cookies[0].Secure {
		t.Fatalf("trusted forwarded proto did not secure cookie: %+v", cookies)
	}

	spoofed := httptest.NewRequest(http.MethodGet, "/", nil)
	spoofed.RemoteAddr = "203.0.113.10:41000"
	spoofed.Header.Set("X-Forwarded-For", "198.51.100.7, 198.51.100.8")
	if got := svc.clientHost(spoofed); got != "198.51.100.8" {
		t.Fatalf("forwarded chain client=%q, want nearest untrusted address", got)
	}
}

func TestPasswordValidationMatchesBcryptLimit(t *testing.T) {
	if err := validatePassword(strings.Repeat("a", 12)); err != nil {
		t.Fatalf("minimum password rejected: %v", err)
	}
	if err := validatePassword(strings.Repeat("é", 36)); err != nil {
		t.Fatalf("72-byte UTF-8 password rejected: %v", err)
	}
	if err := validatePassword(strings.Repeat("é", 37)); err == nil {
		t.Fatal("73-byte UTF-8 password accepted")
	}
	if err := validateCredentials("user", strings.Repeat("a", auth.MaxPasswordBytes+1), "1234"); err == nil {
		t.Fatal("credential validation accepted a bcrypt-overflow password")
	}
}

func TestTOTPReplayAndBackupCodeConcurrency(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/auth.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.CreateUser(ctx, store.User{ID: "totp-user", Username: "totp-user", PasswordHash: "p", PINHash: "p", Role: "user", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	svc := &Server{Store: db}
	if !svc.consumeTOTPReplay(ctx, "totp-user", 100) {
		t.Fatal("first TOTP step was rejected")
	}
	if svc.consumeTOTPReplay(ctx, "totp-user", 100) || svc.consumeTOTPReplay(ctx, "totp-user", 99) {
		t.Fatal("TOTP replay or older step was accepted")
	}
	if !svc.consumeTOTPReplay(ctx, "totp-user", 101) {
		t.Fatal("newer TOTP step was rejected")
	}

	hash, err := auth.Hash("backup-code")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO totp_backup_codes(user_id,code_hash,created_at) VALUES(?,?,?)`, "totp-user", hash, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	results := make(chan bool, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			results <- svc.consumeBackupCode(ctx, "totp-user", "backup-code")
		}()
	}
	group.Wait()
	close(results)
	winners := 0
	for result := range results {
		if result {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("backup-code concurrent winners=%d, want 1", winners)
	}
}
