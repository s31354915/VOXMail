package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/voxmail/voxmail/internal/secret"
	"github.com/voxmail/voxmail/internal/store"
)

func TestSecurityValidationIdentifiesFieldsWithoutExpiringSession(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/security.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	box, err := secret.New("disposable-security-validation-key-123456")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: db, Secrets: box}
	h := s.Handler()
	csrf, cookies := issue(t, h)
	for _, tc := range []struct {
		name, path, field string
		payload           map[string]string
	}{
		{"password/current", "password", "current_password", map[string]string{"current_password": "WrongSensitiveValue-123", "new_password": "ValidNewPassword-123"}},
		{"password/short", "password", "new_password", map[string]string{"current_password": "a-strong-password", "new_password": "short"}},
		{"password/bytes", "password", "new_password", map[string]string{"current_password": "a-strong-password", "new_password": strings.Repeat("é", 40)}},
		{"pin/current", "pin", "current_password", map[string]string{"current_password": "WrongSensitiveValue-123", "new_pin": "2468"}},
		{"pin/length", "pin", "new_pin", map[string]string{"current_password": "a-strong-password", "new_pin": "1"}},
		{"pin/digits", "pin", "new_pin", map[string]string{"current_password": "a-strong-password", "new_pin": "abcde"}},
		{"authenticator/current", "2fa/setup", "current_password", map[string]string{"current_password": "WrongSensitiveValue-123"}},
		{"authenticator/verify-password", "2fa/enable", "current_password", map[string]string{"current_password": "WrongSensitiveValue-123", "secret": "JBSWY3DPEHPK3PXP", "code": "123456"}},
		{"authenticator/code", "2fa/enable", "code", map[string]string{"current_password": "a-strong-password", "secret": "not-base32", "code": "123456"}},
		{"authenticator/disable-password", "2fa/disable", "current_password", map[string]string{"current_password": "WrongSensitiveValue-123", "code": "123456"}},
		{"authenticator/disable-code", "2fa/disable", "code", map[string]string{"current_password": "a-strong-password", "code": "123456"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, _ := json.Marshal(tc.payload)
			r := httptest.NewRequest(http.MethodPost, "/api/v1/security/"+tc.path, bytes.NewReader(data))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("X-CSRF-Token", csrf)
			for _, c := range cookies {
				r.AddCookie(c)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			var failure apiErrorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &failure); err != nil {
				t.Fatal(err)
			}
			if w.Code != 400 || failure.Fields[tc.field] == "" || len(failure.Fields) != 1 || failure.Code != "bad_request" || failure.RequestID == "" {
				t.Fatalf("wrong field validation response: status=%d body=%s", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "WrongSensitiveValue") || strings.Contains(w.Body.String(), "JBSWY3DPEHPK3PXP") {
				t.Fatal("field validation exposed submitted credentials")
			}
			me := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
			for _, c := range cookies {
				me.AddCookie(c)
			}
			mw := httptest.NewRecorder()
			h.ServeHTTP(mw, me)
			if mw.Code != 200 {
				t.Fatalf("validation expired authenticated session: %d", mw.Code)
			}
		})
	}
	// An actually expired session must still be a 401, with no field detail.
	unauth := httptest.NewRecorder()
	h.ServeHTTP(unauth, httptest.NewRequest(http.MethodPost, "/api/v1/security/password", bytes.NewBufferString(`{}`)))
	if unauth.Code != 401 || strings.Contains(unauth.Body.String(), `"fields"`) {
		t.Fatal("missing authentication became a field validation error")
	}
}
