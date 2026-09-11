package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

type fakeAlerts struct {
	calls int
}

func (f *fakeAlerts) TestAlert(context.Context, string) error {
	f.calls++
	return nil
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
	policy := recorder.Header().Get("Content-Security-Policy")
	if !strings.Contains(policy, "media-src 'self' blob:") || strings.Contains(policy, "media-src 'none'") {
		t.Fatalf("unexpected media policy: %q", policy)
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

	request = httptest.NewRequest(http.MethodGet, "/api/v1/sip", nil)
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	recorder = httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	if response := recorder.Result(); response.StatusCode != http.StatusOK {
		t.Fatalf("sip get status %d", response.StatusCode)
	}

	sip := map[string]any{"domain": "sip.example.com", "username": "ada", "password": "hunter2", "port": 5060, "transport": "udp", "reg_interval": 300, "enabled": true}
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
	if st.Domain != "sip.example.com" || !st.Enabled || st.Password == "" {
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
