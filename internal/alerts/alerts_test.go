package alerts

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/voxmail/voxmail/internal/calls"
	"github.com/voxmail/voxmail/internal/secret"
	"github.com/voxmail/voxmail/internal/store"
)

func TestDialURI(t *testing.T) {
	cases := []struct{ phone, domain, want string }{
		{"+15551212", "sip.example.com", "sip:+15551212@sip.example.com"},
		{"+15551212", "", "sip:+15551212"},
		{"sip:+15551212@pbx.example.com", "ignored", "sip:+15551212@pbx.example.com"},
		{"", "sip.example.com", ""},
	}
	for _, c := range cases {
		if got := dialURI(c.phone, c.domain); got != c.want {
			t.Errorf("dialURI(%q,%q) = %q, want %q", c.phone, c.domain, got, c.want)
		}
	}
}

func TestAlertFolderMatch(t *testing.T) {
	if !alertFolderMatch(store.AlertCandidate{Folder: "inbox", AlertFolders: `["INBOX"]`}) {
		t.Fatal("expected INBOX to match inbox")
	}
	if alertFolderMatch(store.AlertCandidate{Folder: "Junk", AlertFolders: `["INBOX"]`}) {
		t.Fatal("expected Junk not to match")
	}
	if alertFolderMatch(store.AlertCandidate{Folder: "INBOX", AlertFolders: `[]`}) {
		t.Fatal("empty alert folder list must never match")
	}
	if alertFolderMatch(store.AlertCandidate{Folder: "INBOX", AlertFolders: `not json`}) {
		t.Fatal("malformed alert folder list must never match")
	}
}

func TestAlertText(t *testing.T) {
	one := []store.AlertCandidate{{Folder: "INBOX", Sender: "Peach", Subject: "Your invoice"}}
	text := alertText(one)
	if !strings.Contains(text, "From Peach") || !strings.Contains(text, "Your invoice") {
		t.Fatalf("unexpected single-alert text %q", text)
	}
	many := append([]store.AlertCandidate{{Folder: "INBOX", Sender: "", Subject: ""}}, one...)
	text = alertText(many)
	if !strings.Contains(text, "2 new email messages") {
		t.Fatalf("unexpected multi-alert text %q", text)
	}
	if !strings.Contains(text, "unknown sender") || !strings.Contains(text, "no subject") {
		t.Fatalf("empty fields not described gracefully: %q", text)
	}
}

type fakeBridge struct {
	mu    sync.Mutex
	dials []calls.DialRequest
	err   error
}

func (f *fakeBridge) Dial(_ context.Context, req calls.DialRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dials = append(f.dials, req)
	if req.Done != nil {
		req.Done <- true
	}
	return f.err
}

func seedAlertStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	box, _ := secret.New("test-key-with-more-than-32-characters-123456")
	ctx := context.Background()
	if err := db.CreateUser(ctx, store.User{ID: "u1", Username: "ada", PasswordHash: "x", PINHash: "y", Role: "user", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `UPDATE settings SET alerts_enabled=1, alert_phone='+15551212' WHERE user_id='u1'`); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertSIP(ctx, box, store.SIPSettings{Domain: "sip.example.com", Username: "ada", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveAccount(ctx, box, store.Account{ID: "a1", UserID: "u1", CanonicalName: "Work", Email: "ada@example.com", SenderName: "Ada", IMAPHost: "imap.example.com", IMAPPort: 993, IMAPUser: "ada", IMAPPassword: "pw", SMTPHost: "smtp.example.com", SMTPPort: 465, SMTPUser: "ada", SMTPPassword: "pw", FolderMap: `{"INBOX":"inbox"}`, AlertFolders: `["INBOX"]`, CallAlertEnabled: true}); err != nil {
		t.Fatal(err)
	}
	mail := []struct {
		folder, path  string
		read, alerted int
	}{
		{"INBOX", "/m/a1/1", 0, 0},
		{"INBOX", "/m/a1/2", 0, 1},
		{"INBOX", "/m/a1/3", 1, 0},
		{"Junk", "/m/a1/4", 0, 0},
	}
	for _, m := range mail {
		if _, err := db.DB.ExecContext(ctx, `INSERT INTO mail_messages(account_id,folder,path,is_read,alerted,updated_at) VALUES(?,?,?,?,?,'now')`, "a1", m.folder, m.path, m.read, m.alerted); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestRoundDialsAndClaims(t *testing.T) {
	db := seedAlertStore(t)
	bridge := &fakeBridge{}
	s := &Service{Store: db, Bridge: bridge, Log: slog.Default(), MinDelay: time.Minute}
	if err := s.round(context.Background()); err != nil {
		t.Fatal(err)
	}
	bridge.mu.Lock()
	dials := len(bridge.dials)
	uri := ""
	if dials > 0 {
		uri = bridge.dials[0].URI
	}
	bridge.mu.Unlock()
	if dials != 1 {
		t.Fatalf("expected a single dial, got %d", dials)
	}
	if uri != "sip:+15551212@sip.example.com" {
		t.Fatalf("unexpected uri %q", uri)
	}
	var candidates []store.AlertCandidate
	var err error
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		candidates, err = s.Store.PendingAlerts(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		claimed := true
		for _, c := range candidates {
			if c.Folder == "INBOX" && c.MessageID == 1 {
				claimed = false
			}
		}
		if claimed {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	for _, c := range candidates {
		if c.Folder == "INBOX" && c.MessageID == 1 {
			t.Fatalf("message was not claimed after dial: %+v", c)
		}
	}
}

func TestTestAlertRequiresPhone(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.CreateUser(ctx, store.User{ID: "u2", Username: "bob", PasswordHash: "x", PINHash: "y", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	bridge := &fakeBridge{}
	s := &Service{Store: db, Bridge: bridge}
	if err := s.TestAlert(ctx, "u2"); err == nil || !strings.Contains(err.Error(), "alert phone") {
		t.Fatalf("expected missing-phone error, got %v", err)
	}
	if _, err := db.DB.ExecContext(ctx, `UPDATE settings SET alert_phone='+15559999' WHERE user_id='u2'`); err != nil {
		t.Fatal(err)
	}
	if err := s.TestAlert(ctx, "u2"); err == nil || !strings.Contains(err.Error(), "SIP registrar") {
		t.Fatalf("expected missing-domain error, got %v", err)
	}
	bridge.mu.Lock()
	before := len(bridge.dials)
	bridge.mu.Unlock()
	if before != 0 {
		t.Fatalf("expected no dials, got %d", before)
	}
}
