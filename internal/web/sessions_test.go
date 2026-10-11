package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/voxmail/voxmail/internal/auth"
)

func TestSessionStoreBoundsConcurrentPerUserCreation(t *testing.T) {
	s := NewSessionStore()
	var created atomic.Int32
	var group sync.WaitGroup
	for i := range 500 {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			if s.Put(fmt.Sprint("token-", i), Session{UserID: "one-user", Expires: time.Now().Add(time.Hour)}) {
				created.Add(1)
			}
		}(i)
	}
	group.Wait()
	if int(created.Load()) != sessionMaxPerUser || len(s.byToken) != sessionMaxPerUser || s.byUser["one-user"] != sessionMaxPerUser {
		t.Fatalf("per-user cap: created=%d tokens=%d users=%v", created.Load(), len(s.byToken), s.byUser)
	}
	if !s.Put("other-user", Session{UserID: "other-user", Expires: time.Now().Add(time.Hour)}) {
		t.Fatal("one user's sessions denied another user's capacity")
	}
	s.DeleteUser("one-user")
	if len(s.byToken) != 1 || len(s.byUser) != 1 {
		t.Fatalf("user deletion leaked quota: tokens=%d users=%v", len(s.byToken), s.byUser)
	}
}

func TestSessionStoreGlobalCapExpiryAndReplacement(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	s := NewSessionStore()
	s.now = func() time.Time { return now }
	for i := range sessionMaxTotal {
		if !s.Put(fmt.Sprint("token-", i), Session{UserID: fmt.Sprint("user-", i), Expires: now.Add(time.Hour)}) {
			t.Fatalf("capacity rejected at %d", i)
		}
	}
	var group sync.WaitGroup
	for i := range 1000 {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			if s.Put(fmt.Sprint("excess-", i), Session{UserID: fmt.Sprint("excess-user-", i), Expires: now.Add(time.Hour)}) {
				t.Error("global cap admitted an excess session")
			}
		}(i)
	}
	group.Wait()
	if len(s.byToken) != sessionMaxTotal || len(s.byUser) != sessionMaxTotal {
		t.Fatal("excess sessions grew storage")
	}
	if _, ok := s.Get("token-0"); !ok {
		t.Fatal("overflow evicted an existing session")
	}
	if !s.Put("token-0", Session{UserID: "replacement-user", Expires: now.Add(time.Hour)}) {
		t.Fatal("replacement of an existing token needed extra capacity")
	}
	if _, exists := s.byUser["user-0"]; exists || s.byUser["replacement-user"] != 1 {
		t.Fatal("token replacement corrupted user quotas")
	}
	now = now.Add(time.Hour)
	if !s.Put("fresh-token", Session{UserID: "fresh-user", Expires: now.Add(time.Hour)}) {
		t.Fatal("expired sessions did not release capacity")
	}
	if len(s.byToken) != 1 || len(s.byUser) != 1 {
		t.Fatalf("expiry retained stale quota: tokens=%d users=%d", len(s.byToken), len(s.byUser))
	}
	now = now.Add(time.Hour)
	if _, ok := s.Get("fresh-token"); ok || len(s.byUser) != 0 {
		t.Fatal("Get accepted an expired token or retained its quota")
	}
}

func TestSessionGrantIsUndisclosedUntilCommitAndRollbackReleasesCapacity(t *testing.T) {
	s := &Server{Sessions: NewSessionStore()}
	w := httptest.NewRecorder()
	grant, ok := s.reserveSession(w, "user")
	if !ok || w.Header().Get("Set-Cookie") != "" || w.Header().Get("X-CSRF-Token") != "" {
		t.Fatal("reservation disclosed a session before authentication completed")
	}
	grant.cancel()
	grant.cancel()
	if len(s.Sessions.byToken) != 0 || len(s.Sessions.byUser) != 0 {
		t.Fatal("rollback leaked session capacity")
	}
	grant, ok = s.reserveSession(w, "user")
	if !ok {
		t.Fatal("replacement grant rejected")
	}
	grant.commit(w, httptest.NewRequest(http.MethodPost, "/api/v1/login", nil))
	grant.cancel()
	if _, exists := s.Sessions.Get(grant.token); !exists || len(w.Result().Cookies()) != 1 {
		t.Fatal("commit did not retain and disclose the authenticated session")
	}
}

func TestRevokedSessionGrantCannotReportSuccessfulAuthentication(t *testing.T) {
	s := &Server{Sessions: NewSessionStore()}
	w := httptest.NewRecorder()
	grant, ok := s.reserveSession(w, "user")
	if !ok {
		t.Fatal("reservation failed")
	}
	s.Sessions.DeleteUser("user")
	if grant.commit(w, httptest.NewRequest(http.MethodPost, "/api/v1/login", nil)) ||
		w.Code != http.StatusUnauthorized || w.Header().Get("Set-Cookie") != "" {
		t.Fatal("revoked reservation was reported as authenticated")
	}
}

func TestSessionReplacementKeepsQuotaDuringConcurrentCreation(t *testing.T) {
	s := NewSessionStore()
	expires := time.Now().Add(time.Hour)
	for i := range sessionMaxTotal {
		if !s.Put(fmt.Sprint("original-", i), Session{UserID: fmt.Sprint("user-", i), Expires: expires}) {
			t.Fatal("fixture capacity rejected")
		}
	}
	var group sync.WaitGroup
	for i := range 100 {
		group.Add(2)
		go func(i int) {
			defer group.Done()
			if !s.replaceUser(fmt.Sprint("rotated-", i), Session{UserID: "user-0", Expires: expires}) {
				t.Error("replacement lost its reserved quota")
			}
		}(i)
		go func(i int) {
			defer group.Done()
			if s.Put(fmt.Sprint("excess-", i), Session{UserID: "excess-user", Expires: expires}) {
				t.Error("concurrent creation stole the replacement quota")
			}
		}(i)
	}
	group.Wait()
	if s.byUser["user-0"] != 1 || len(s.byToken) != sessionMaxTotal {
		t.Fatal("replacement corrupted session counts")
	}
	if _, exists := s.Get("original-1"); !exists {
		t.Fatal("replacement evicted another user's session")
	}
}

func fillUserSessions(t *testing.T, s *SessionStore, userID string) {
	t.Helper()
	for i := range sessionMaxPerUser {
		if !s.Put(fmt.Sprint("existing-", i), Session{UserID: userID, Expires: time.Now().Add(time.Hour)}) {
			t.Fatalf("fixture session %d rejected", i)
		}
	}
}

func TestLoginSessionCapacityDoesNotConsumeBackupCode(t *testing.T) {
	s := loginGuardFixture(t)
	s.loginCheck = func(_, _ string) bool { return true }
	codeHash, err := auth.Hash("backup-code")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Store.EnableTOTP(context.Background(), "user-0", "JBSWY3DPEHPK3PXP", []string{codeHash}, time.Now()); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	fillUserSessions(t, s.Sessions, "user-0")
	login := func() *httptest.ResponseRecorder {
		payload, _ := json.Marshal(loginRequest{Username: "user-0", Password: "correct", BackupCode: "backup-code"})
		r := httptest.NewRequest(http.MethodPost, "/api/v1/login", bytes.NewReader(payload))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := login(); w.Code != http.StatusTooManyRequests || len(w.Result().Cookies()) != 0 {
		t.Fatalf("full session store status=%d cookies=%v", w.Code, w.Result().Cookies())
	}
	s.Sessions.Delete("existing-0")
	if w := login(); w.Code != http.StatusOK {
		t.Fatalf("capacity rejection consumed backup code: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestRecoverySessionCapacityDoesNotConsumeOneUseToken(t *testing.T) {
	s, db, userID := newRecoveryFixture(t, true, false)
	h := s.Handler()
	insertRecoveryToken(t, db, userID, "recovery@example.com", "bypass_2fa", "123456", time.Now().Add(10*time.Minute))
	fillUserSessions(t, s.Sessions, userID)
	redeem := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/recovery/confirm", bytes.NewBufferString(`{"email":"recovery@example.com","purpose":"bypass_2fa","code":"123456"}`))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := redeem(); w.Code != http.StatusTooManyRequests || len(w.Result().Cookies()) != 0 {
		t.Fatalf("full session store status=%d cookies=%v", w.Code, w.Result().Cookies())
	}
	s.Sessions.Delete("existing-0")
	if w := redeem(); w.Code != http.StatusOK {
		t.Fatalf("capacity rejection consumed recovery token: status=%d body=%s", w.Code, w.Body.String())
	}
}
