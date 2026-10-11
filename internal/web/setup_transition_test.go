package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/voxmail/voxmail/internal/store"
)

func TestBootstrapCanTransitionToLoginWithoutCreatingSession(t *testing.T) {
	for _, test := range []struct {
		name         string
		signIn       any
		wantsSession bool
	}{
		{"console transition", false, false}, {"legacy API", nil, true}, {"explicit sign-in", true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, err := store.Open(filepath.Join(t.TempDir(), "setup.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			srv := &Server{Store: db, setupHash: func(string) (string, error) { return "fixture-hash", nil }}
			h := srv.Handler()
			defer srv.Close()
			body := map[string]any{"username": "first-admin", "password": "BootstrapPassword-123", "pin": "2468"}
			if test.signIn != nil {
				body["sign_in"] = test.signIn
			}
			response := voiceRequest(h, "", http.MethodPost, "/api/v1/setup", body)
			if response.Code != http.StatusCreated {
				t.Fatal(response.Code, response.Body.String())
			}
			user, err := db.UserByUsername(context.Background(), "first-admin")
			if err != nil || user.Role != "admin" || !user.Enabled {
				t.Fatal("bootstrap user not created", user, err)
			}
			hasSession := false
			request := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
			for _, cookie := range response.Result().Cookies() {
				request.AddCookie(cookie)
				if cookie.Name == "voxmail_session" {
					hasSession = true
				}
			}
			if hasSession != test.wantsSession {
				t.Fatal("unexpected bootstrap session", hasSession)
			}
			me := httptest.NewRecorder()
			h.ServeHTTP(me, request)
			want := http.StatusUnauthorized
			if test.wantsSession {
				want = http.StatusOK
			}
			if me.Code != want {
				t.Fatal("bootstrap authentication state", me.Code, me.Body.String())
			}
			if response := voiceRequest(h, "", http.MethodGet, "/api/v1", nil); response.Code != http.StatusOK {
				t.Fatal(response.Code)
			}
		})
	}
}
