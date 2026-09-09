package calls

import (
	"bufio"
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/voxmail/voxmail/internal/bridge"
	"github.com/voxmail/voxmail/internal/store"
)

func newTestService(t *testing.T) (*Service, *store.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	svc := &Service{Store: db, MaxCalls: 1, sessions: make(map[string]*session), pinLock: make(map[string]time.Time)}
	return svc, db
}

func admitAndRead(t *testing.T, svc *Service, message bridge.Message) bridge.Message {
	t.Helper()
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	done := make(chan error, 1)
	go func() { done <- svc.admit(server, message) }()
	client.SetReadDeadline(time.Now().Add(5 * time.Second))
	out, err := bridge.Decode(bufio.NewReader(client))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("admit: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("admit did not return")
	}
	return out
}

func TestAdmitAuthorizedAnswers(t *testing.T) {
	svc, db := newTestService(t)
	if err := db.CreateUser(context.Background(), store.User{ID: "u1", Username: "alice", PasswordHash: "x", PINHash: "y", Role: "admin", Enabled: true}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := db.AddWhitelist(context.Background(), store.WhitelistEntry{UserID: "u1", Phone: "+15551234567"}); err != nil {
		t.Fatalf("AddWhitelist: %v", err)
	}
	out := admitAndRead(t, svc, bridge.Message{Type: "call_incoming", CallID: "call-1", From: "sip:+15551234567@example.com"})
	if out.Type != "answer" || out.UserID != "u1" {
		t.Fatalf("expected answer for u1, got %+v", out)
	}
	svc.mu.Lock()
	sess := svc.sessions["call-1"]
	svc.mu.Unlock()
	if sess == nil || sess.Phone != "+15551234567" {
		t.Fatalf("session not recorded correctly: %+v", sess)
	}
}

func TestAdmitUnauthorizedHangsUp(t *testing.T) {
	svc, _ := newTestService(t)
	out := admitAndRead(t, svc, bridge.Message{Type: "call_incoming", CallID: "call-1", From: "sip:+15550000000@example.com"})
	if out.Type != "hangup" || out.Code != 603 {
		t.Fatalf("expected hangup 603, got %+v", out)
	}
}

func TestAdmitBusy(t *testing.T) {
	svc, db := newTestService(t)
	phone := "+15551234567"
	if err := db.CreateUser(context.Background(), store.User{ID: "u1", Username: "alice", PasswordHash: "x", PINHash: "y", Role: "admin", Enabled: true}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := db.AddWhitelist(context.Background(), store.WhitelistEntry{UserID: "u1", Phone: phone}); err != nil {
		t.Fatalf("AddWhitelist: %v", err)
	}
	svc.mu.Lock()
	svc.sessions["call-occupied"] = &session{CallID: "call-occupied", UserID: "u1", Phone: phone}
	svc.mu.Unlock()
	out := admitAndRead(t, svc, bridge.Message{Type: "call_incoming", CallID: "call-2", From: phone})
	if out.Type != "hangup" || out.Code != 486 {
		t.Fatalf("expected hangup 486, got %+v", out)
	}
}

func TestAdmitLockedOut(t *testing.T) {
	svc, _ := newTestService(t)
	svc.recordPinFailure("+15551234567")
	out := admitAndRead(t, svc, bridge.Message{Type: "call_incoming", CallID: "call-1", From: "sip:+15551234567@example.com"})
	if out.Type != "hangup" || out.Code != 603 || out.Reason != "caller is temporarily locked out" {
		t.Fatalf("expected lockout hangup, got %+v", out)
	}
}

func TestPinCooldownLifecycle(t *testing.T) {
	svc, _ := newTestService(t)
	phone := "+15551234567"
	if !svc.pinLockedUntil(phone).IsZero() {
		t.Fatal("phone locked before any failure")
	}
	svc.recordPinFailure(phone)
	if svc.pinLockedUntil(phone).IsZero() {
		t.Fatal("phone not locked after failure")
	}
	svc.clearPinFailure(phone)
	if !svc.pinLockedUntil(phone).IsZero() {
		t.Fatal("phone still locked after clear")
	}

	svc.recordPinFailure(phone)
	svc.pinMu.Lock()
	svc.pinLock[phone] = time.Now().Add(-time.Second)
	svc.pinMu.Unlock()
	if !svc.pinLockedUntil(phone).IsZero() {
		t.Fatal("expired lockout did not clear")
	}
	if _, ok := svc.pinLock[phone]; ok {
		t.Fatal("expired lockout entry was not removed")
	}
}

func TestNormalizePhone(t *testing.T) {
	cases := map[string]string{
		"sip:+15551234567@example.com": "+15551234567",
		"+15551234567":                 "+15551234567",
		"  +15551234567  ":             "+15551234567",
		"sip:1000@pbx.example:5060":    "1000",
		"1000;tag=1234":                "1000",
		"":                             "",
	}
	for input, want := range cases {
		if got := normalizePhone(input); got != want {
			t.Errorf("normalizePhone(%q) = %q, want %q", input, got, want)
		}
	}
}
