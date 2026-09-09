package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

	svc.Alerts = nil
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
