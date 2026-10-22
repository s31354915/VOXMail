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

func newUsernameTestServer(t *testing.T) (http.Handler, string, []*http.Cookie) {
	t.Helper()
	db, err := store.Open(t.TempDir() + "/username.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	box, err := secret.New("disposable-username-change-key-123456")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: db, Secrets: box}
	h := s.Handler()
	csrf, cookies := issue(t, h)
	return h, csrf, cookies
}

func postUsernameChange(t *testing.T, h http.Handler, csrf string, cookies []*http.Cookie, payload string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/security/username", bytes.NewBufferString(payload))
	r.Header.Set("Content-Type", "application/json")
	if csrf != "" {
		r.Header.Set("X-CSRF-Token", csrf)
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestChangeUsernameValidatesFieldsWithoutExpiringSession(t *testing.T) {
	h, csrf, cookies := newUsernameTestServer(t)
	for _, tc := range []struct {
		name, payload, field string
	}{
		{"wrong-password", `{"current_password":"WrongSensitiveValue-123","new_username":"renamed"}`, "current_password"},
		{"short", `{"current_password":"a-strong-password","new_username":"ab"}`, "new_username"},
		{"long", `{"current_password":"a-strong-password","new_username":"` + strings.Repeat("x", 65) + `"}`, "new_username"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := postUsernameChange(t, h, csrf, cookies, tc.payload)
			var failure apiErrorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &failure); err != nil {
				t.Fatal(err)
			}
			if w.Code != 400 || failure.Fields[tc.field] == "" || len(failure.Fields) != 1 || failure.Code != "bad_request" || failure.RequestID == "" {
				t.Fatalf("wrong field validation response: status=%d body=%s", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "WrongSensitiveValue") {
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
	unauth := httptest.NewRecorder()
	h.ServeHTTP(unauth, httptest.NewRequest(http.MethodPost, "/api/v1/security/username", bytes.NewBufferString(`{}`)))
	if unauth.Code != 401 || strings.Contains(unauth.Body.String(), `"fields"`) {
		t.Fatal("missing authentication became a field validation error")
	}
}

func TestChangeUsernameChangesAccountAndRejectsTaken(t *testing.T) {
	h, csrf, cookies := newUsernameTestServer(t)

	w := postUsernameChange(t, h, csrf, cookies, `{"current_password":"a-strong-password","new_username":"renamed-admin"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("change status=%d body=%s", w.Code, w.Body.String())
	}
	reCookies := w.Result().Cookies()
	if len(reCookies) == 0 {
		t.Fatal("username change did not issue a replacement session")
	}
	// The old session must be gone, the replacement must carry the new name.
	me := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	for _, c := range cookies {
		me.AddCookie(c)
	}
	oldMe := httptest.NewRecorder()
	h.ServeHTTP(oldMe, me)
	if oldMe.Code != http.StatusUnauthorized {
		t.Fatalf("stale session after rename status=%d, want 401", oldMe.Code)
	}
	me = httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	for _, c := range reCookies {
		me.AddCookie(c)
	}
	newMe := httptest.NewRecorder()
	h.ServeHTTP(newMe, me)
	if newMe.Code != http.StatusOK {
		t.Fatalf("replacement session /me status=%d, want 200", newMe.Code)
	}
	var user map[string]any
	if err := json.NewDecoder(newMe.Body).Decode(&user); err != nil {
		t.Fatal(err)
	}
	if user["username"] != "renamed-admin" {
		t.Fatalf("renamed user=%v", user)
	}
	// A fresh csrf token for the follow-up posts on the rotated session.
	newCSRF := newMe.Header().Get("X-CSRF-Token")
	if newCSRF == "" {
		t.Fatal("replacement session did not return a csrf token")
	}

	// Seed a second account through the admin API to test the uniqueness rule.
	create := httptest.NewRequest(http.MethodPost, "/api/v1/users", bytes.NewBufferString(`{"username":"second-user","password":"a-strong-password","pin":"1234","role":"user"}`))
	create.Header.Set("Content-Type", "application/json")
	create.Header.Set("X-CSRF-Token", newCSRF)
	for _, c := range reCookies {
		create.AddCookie(c)
	}
	cw := httptest.NewRecorder()
	h.ServeHTTP(cw, create)
	if cw.Code != http.StatusCreated {
		t.Fatalf("create second user status=%d body=%s", cw.Code, cw.Body.String())
	}

	taken := postUsernameChange(t, h, newCSRF, reCookies, `{"current_password":"a-strong-password","new_username":"second-user"}`)
	var failure apiErrorResponse
	if err := json.Unmarshal(taken.Body.Bytes(), &failure); err != nil {
		t.Fatal(err)
	}
	if taken.Code != 400 || failure.Fields["new_username"] == "" || !strings.Contains(failure.Fields["new_username"], "already used") {
		t.Fatalf("taken username not rejected: status=%d body=%s", taken.Code, taken.Body.String())
	}
	own := postUsernameChange(t, h, newCSRF, reCookies, `{"current_password":"a-strong-password","new_username":"renamed-admin"}`)
	if own.Code != http.StatusOK {
		t.Fatalf("keeping own username status=%d body=%s", own.Code, own.Body.String())
	}
}
