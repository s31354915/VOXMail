package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/voxmail/voxmail/internal/store"
)

// TestProtectedRouteAuthenticationCharacterization records the pre-refactor
// boundary contract for every authenticated route. It intentionally exercises
// the mux through Handler rather than calling handlers directly, so a future
// split cannot accidentally change route registration or move authentication
// below request decoding or business work.
func TestProtectedRouteAuthenticationCharacterization(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "characterization.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h := (&Server{Store: db}).Handler()
	cases := []struct {
		name   string
		method string
		path   string
	}{
		{name: "password", method: http.MethodPost, path: "/api/v1/security/password"},
		{name: "pin", method: http.MethodPost, path: "/api/v1/security/pin"},
		{name: "2fa setup", method: http.MethodPost, path: "/api/v1/security/2fa/setup"},
		{name: "2fa enable", method: http.MethodPost, path: "/api/v1/security/2fa/enable"},
		{name: "2fa disable", method: http.MethodPost, path: "/api/v1/security/2fa/disable"},
		{name: "me", method: http.MethodGet, path: "/api/v1/me"},
		{name: "users", method: http.MethodGet, path: "/api/v1/users"},
		{name: "create user", method: http.MethodPost, path: "/api/v1/users"},
		{name: "delete user", method: http.MethodDelete, path: "/api/v1/users/user-id"},
		{name: "accounts", method: http.MethodGet, path: "/api/v1/accounts"},
		{name: "save account", method: http.MethodPost, path: "/api/v1/accounts"},
		{name: "account order", method: http.MethodPut, path: "/api/v1/accounts/order"},
		{name: "validate account", method: http.MethodPost, path: "/api/v1/accounts/validate"},
		{name: "test account", method: http.MethodPost, path: "/api/v1/accounts/test"},
		{name: "sync account", method: http.MethodPost, path: "/api/v1/accounts/account-id/sync"},
		{name: "account mutations", method: http.MethodGet, path: "/api/v1/accounts/account-id/mutations"},
		{name: "delete account", method: http.MethodDelete, path: "/api/v1/accounts/account-id"},
		{name: "contacts", method: http.MethodGet, path: "/api/v1/contacts"},
		{name: "create contact", method: http.MethodPost, path: "/api/v1/contacts"},
		{name: "update contact", method: http.MethodPut, path: "/api/v1/contacts/1"},
		{name: "delete contact", method: http.MethodDelete, path: "/api/v1/contacts/1"},
		{name: "whitelist", method: http.MethodGet, path: "/api/v1/whitelist"},
		{name: "add whitelist", method: http.MethodPost, path: "/api/v1/whitelist"},
		{name: "delete whitelist", method: http.MethodDelete, path: "/api/v1/whitelist/1"},
		{name: "settings", method: http.MethodGet, path: "/api/v1/settings"},
		{name: "save settings", method: http.MethodPut, path: "/api/v1/settings"},
		{name: "admin alerts", method: http.MethodGet, path: "/api/v1/admin/alerts"},
		{name: "save admin alerts", method: http.MethodPut, path: "/api/v1/admin/alerts"},
		{name: "alert numbers", method: http.MethodGet, path: "/api/v1/alert-numbers"},
		{name: "save alert number", method: http.MethodPost, path: "/api/v1/alert-numbers"},
		{name: "activate alert number", method: http.MethodPut, path: "/api/v1/alert-numbers/1/active"},
		{name: "delete alert number", method: http.MethodDelete, path: "/api/v1/alert-numbers/1"},
		{name: "voices", method: http.MethodGet, path: "/api/v1/voices"},
		{name: "install voice", method: http.MethodPost, path: "/api/v1/voices/install"},
		{name: "voice job", method: http.MethodGet, path: "/api/v1/voices/jobs/job-id"},
		{name: "preview voice", method: http.MethodPost, path: "/api/v1/voices/preview"},
		{name: "test alert", method: http.MethodPost, path: "/api/v1/alerts/test"},
		{name: "sip", method: http.MethodGet, path: "/api/v1/sip"},
		{name: "save sip", method: http.MethodPut, path: "/api/v1/sip"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			h.ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.path, nil))
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s returned %d, want %d", tc.method, tc.path, recorder.Code, http.StatusUnauthorized)
			}
			if recorder.Header().Get("X-Request-ID") == "" {
				t.Fatal("unauthorized response missing request ID")
			}
		})
	}

	// Keep the public boundary explicit as well: these endpoints must remain
	// usable before the first account/session exists.
	public := []struct {
		name, method, path string
	}{
		{name: "health", method: http.MethodGet, path: "/healthz"},
		{name: "ready", method: http.MethodGet, path: "/readyz"},
		{name: "api info", method: http.MethodGet, path: "/api/v1"},
		{name: "setup", method: http.MethodPost, path: "/api/v1/setup"},
		{name: "login", method: http.MethodPost, path: "/api/v1/login"},
		{name: "recovery request", method: http.MethodPost, path: "/api/v1/recovery/request"},
		{name: "recovery confirm", method: http.MethodPost, path: "/api/v1/recovery/confirm"},
		{name: "logout", method: http.MethodPost, path: "/api/v1/logout"},
		{name: "index", method: http.MethodGet, path: "/"},
	}
	for _, tc := range public {
		t.Run("public/"+tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			h.ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.path, nil))
			if recorder.Code == http.StatusUnauthorized {
				t.Fatalf("public %s %s unexpectedly required authentication", tc.method, tc.path)
			}
		})
	}
}
