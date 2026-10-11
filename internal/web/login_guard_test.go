package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/voxmail/voxmail/internal/store"
)

func TestLoginReservesLastSourceAttemptBeforeVerification(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	s := &Server{loginNow: func() time.Time { return now }}
	for range loginFailureLimit - 1 {
		a, retry := s.reserveLoginAttempt("source", "victim")
		if retry != 0 {
			t.Fatal(retry)
		}
		a.finish(false)
	}
	var group sync.WaitGroup
	admitted := make(chan *loginAttempt, 100)
	for range 100 {
		group.Add(1)
		go func() {
			defer group.Done()
			if a, retry := s.reserveLoginAttempt("source", "victim"); retry == 0 {
				admitted <- a
			}
		}()
	}
	group.Wait()
	if len(admitted) != 1 {
		t.Fatalf("simultaneous attempts bypassed failure budget: admitted=%d", len(admitted))
	}
	a := <-admitted
	a.finish(false)
	a.finish(false) // release is idempotent
	if _, retry := s.reserveLoginAttempt("source", "victim"); retry != loginCooldown {
		t.Fatalf("cooldown=%s", retry)
	}
	now = now.Add(loginCooldown)
	a, retry := s.reserveLoginAttempt("source", "victim")
	if retry != 0 {
		t.Fatalf("cooldown did not expire: %s", retry)
	}
	a.finish(true)
	if s.loginActive != 0 || len(s.loginAccounts) != 0 {
		t.Fatal("login reservation leaked")
	}
}

func TestLoginBoundsAccountConcurrencyWithoutAccountCooldown(t *testing.T) {
	s := &Server{}
	var held []*loginAttempt
	for i := range loginAccountConcurrency {
		a, retry := s.reserveLoginAttempt(fmt.Sprint("attacker-", i), "victim")
		if retry != 0 {
			t.Fatal(retry)
		}
		held = append(held, a)
	}
	if _, retry := s.reserveLoginAttempt("third", "victim"); retry == 0 {
		t.Fatal("account concurrency cap was bypassed")
	}
	other, retry := s.reserveLoginAttempt("legitimate", "other-user")
	if retry != 0 {
		t.Fatalf("targeted account consumed every slot: %s", retry)
	}
	other.finish(true)
	for _, a := range held {
		a.finish(false)
	}
	legitimate, retry := s.reserveLoginAttempt("legitimate", "victim")
	if retry != 0 {
		t.Fatalf("other-source failures locked victim account: %s", retry)
	}
	legitimate.finish(true)
}

func TestLoginGlobalRateAndCardinalityBudgets(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	s := &Server{loginNow: func() time.Time { return now }}
	for i := range loginGlobalLimit {
		a, retry := s.reserveLoginAttempt(fmt.Sprint("source-", i), fmt.Sprint("user-", i))
		if retry != 0 {
			t.Fatal(retry)
		}
		a.finish(true)
	}
	if _, retry := s.reserveLoginAttempt("new-source", "new-account"); retry != loginGlobalWindow {
		t.Fatalf("distributed sources bypassed global rate: retry=%s", retry)
	}
	for i := loginGlobalLimit; i < 6000; i++ {
		if i%loginGlobalLimit == 0 {
			now = now.Add(loginGlobalWindow)
		}
		if a, retry := s.reserveLoginAttempt(fmt.Sprint("source-", i), fmt.Sprint("user-", i)); retry == 0 {
			a.finish(false)
		}
	}
	if len(s.logins) != loginMaxStates || len(s.loginAccounts) != 0 || s.loginActive != 0 {
		t.Fatalf("cardinality state: keys=%d accounts=%d active=%d", len(s.logins), len(s.loginAccounts), s.loginActive)
	}
	for i := range 10000 {
		if _, retry := s.reserveLoginAttempt("source-cardinality", fmt.Sprint("unknown-", i)); retry == 0 {
			t.Fatal("full limiter admitted an untracked name")
		}
	}
	if len(s.logins) != loginMaxStates {
		t.Fatal("rejected requests grew limiter state")
	}
	now = now.Add(loginStateLifetime + loginReapInterval)
	a, retry := s.reserveLoginAttempt("fresh-source", "fresh-user")
	if retry != 0 {
		t.Fatalf("expired state did not release capacity: %s", retry)
	}
	a.finish(true)
	if len(s.logins) != 2 {
		t.Fatalf("expired keys retained: %d", len(s.logins))
	}
}

func loginGuardFixture(t *testing.T) *Server {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "login.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for i := range loginHashConcurrency {
		if err := db.CreateUser(context.Background(), store.User{
			ID: fmt.Sprint("user-", i), Username: fmt.Sprint("user-", i),
			PasswordHash: "trusted-fixture-hash", PINHash: "trusted-fixture-pin", Role: "user", Enabled: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return &Server{Store: db, Sessions: NewSessionStore()}
}

func guardLoginRequest(h http.Handler, username, password, source string) *httptest.ResponseRecorder {
	payload, _ := json.Marshal(loginRequest{Username: username, Password: password})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/login", bytes.NewReader(payload))
	r.Header.Set("Content-Type", "application/json")
	r.RemoteAddr = source + ":12345"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestLoginHTTPBoundsSimultaneousPasswordVerification(t *testing.T) {
	s := loginGuardFixture(t)
	entered := make(chan struct{}, loginHashConcurrency)
	release := make(chan struct{})
	var calls atomic.Int32
	s.loginCheck = func(hash, value string) bool {
		calls.Add(1)
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-time.After(3 * time.Second):
		}
		return false
	}
	h := s.Handler()
	var held sync.WaitGroup
	for i := range loginHashConcurrency {
		held.Add(1)
		go func(i int) {
			defer held.Done()
			if w := guardLoginRequest(h, fmt.Sprint("user-", i), "wrong", "192.0.2.1"); w.Code != http.StatusUnauthorized {
				t.Errorf("admitted login status=%d", w.Code)
			}
		}(i)
	}
	defer held.Wait()
	defer close(release)
	for range loginHashConcurrency {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("password check did not begin")
		}
	}
	var rejected sync.WaitGroup
	for i := range 100 {
		rejected.Add(1)
		go func(i int) {
			defer rejected.Done()
			w := guardLoginRequest(h, "user-0", "wrong", fmt.Sprintf("198.51.100.%d", i+1))
			if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
				t.Errorf("excess login status=%d retry=%q", w.Code, w.Header().Get("Retry-After"))
			}
		}(i)
	}
	rejected.Wait()
	if calls.Load() != loginHashConcurrency {
		t.Fatalf("hash work exceeded concurrency budget: calls=%d", calls.Load())
	}
}

func TestDistributedFailuresDoNotLockAccountOrEvictSessions(t *testing.T) {
	s := loginGuardFixture(t)
	s.loginCheck = func(_, password string) bool { return password == "correct" }
	h := s.Handler()
	if !s.Sessions.Put("existing-session", Session{UserID: "user-0", Expires: time.Now().Add(time.Hour)}) {
		t.Fatal("fixture session rejected")
	}
	for i := range 100 {
		if w := guardLoginRequest(h, "user-0", "wrong", fmt.Sprintf("198.51.100.%d", i+1)); w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status=%d", i, w.Code)
		}
	}
	w := guardLoginRequest(h, "user-0", "correct", "203.0.113.5")
	if w.Code != http.StatusOK || len(w.Result().Cookies()) != 1 {
		t.Fatalf("distributed attack blocked valid login: status=%d body=%s", w.Code, w.Body.String())
	}
	if _, exists := s.Sessions.Get("existing-session"); !exists {
		t.Fatal("failed logins evicted a legitimate session")
	}
}
