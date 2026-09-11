package calls

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/voxmail/voxmail/internal/bridge"
	"github.com/voxmail/voxmail/internal/ivr"
	"github.com/voxmail/voxmail/internal/keypad"
	"github.com/voxmail/voxmail/internal/mailer"
	"github.com/voxmail/voxmail/internal/store"
)

func TestForwardStartsRecipientEntryAndPreservesOriginal(t *testing.T) {
	svc, _ := newTestService(t)
	rawPath := filepath.Join(t.TempDir(), "original.eml")
	if err := os.WriteFile(rawPath, []byte("From: sender@example.com\r\n\r\noriginal"), 0600); err != nil {
		t.Fatal(err)
	}
	sess := &session{
		Messages: []store.MailSummary{{Path: rawPath, Sender: "Sender <sender@example.com>", Subject: "Original subject"}},
		Cursor:   0,
	}
	svc.startCompose(sess, "forward")
	if sess.State != "compose" {
		t.Fatalf("state=%q, want compose", sess.State)
	}
	if sess.Draft.To != "" {
		t.Fatalf("forward unexpectedly prefilled recipient %q", sess.Draft.To)
	}
	if !sess.Draft.ForwardOriginal || len(sess.Draft.Attachments) != 1 {
		t.Fatalf("original forwarding not preserved: %+v", sess.Draft)
	}
	if string(sess.Draft.Attachments[0].Data) != "From: sender@example.com\r\n\r\noriginal" {
		t.Fatal("forwarded raw message was not retained")
	}
	if sess.Editor == nil {
		t.Fatal("forward did not initialize recipient editor")
	}
}

func TestKeypadFieldsRequireConfirmation(t *testing.T) {
	svc, _ := newTestService(t)
	sess := &session{State: "compose", Editor: keypad.New(keypad.ModeEmail)}
	sess.Editor.Text = "person@example.com"
	svc.handleCompose(sess, "#")
	if sess.State != "field_confirm" || sess.ConfirmValue != "person@example.com" {
		t.Fatalf("recipient was committed before confirmation: state=%q value=%q", sess.State, sess.ConfirmValue)
	}
	svc.handleFieldConfirmation(sess, "2")
	if sess.State != "compose" || sess.Editor == nil || sess.Draft.To != "" {
		t.Fatalf("re-entry did not clear the pending recipient: state=%q editor=%v to=%q", sess.State, sess.Editor != nil, sess.Draft.To)
	}
	sess.Editor.Text = "person@example.com"
	svc.handleCompose(sess, "#")
	svc.handleFieldConfirmation(sess, "1")
	if sess.State != "recipient_menu" || sess.Draft.To != "person@example.com" {
		t.Fatalf("recipient confirmation failed: state=%q to=%q", sess.State, sess.Draft.To)
	}
}

func TestPromptFIFOOpenRespectsCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-reader.pcm")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	err := (&PromptPlayer{Binary: "does-not-matter"}).playInput(ctx, path, "input.wav")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("playInput error=%v, want context deadline", err)
	}
}

func TestPlayMediaStreamsPCMToCallFIFO(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "call-tx.pcm")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	ffmpeg := filepath.Join(dir, "fake-ffmpeg")
	if err := os.WriteFile(ffmpeg, []byte("#!/bin/sh\nprintf '\\001\\002\\003\\004'\n"), 0700); err != nil {
		t.Fatal(err)
	}

	readResult := make(chan []byte, 1)
	readError := make(chan error, 1)
	go func() {
		file, err := os.Open(fifo)
		if err != nil {
			readError <- err
			return
		}
		defer file.Close()
		data, err := io.ReadAll(file)
		if err != nil {
			readError <- err
			return
		}
		readResult <- data
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := (&PromptPlayer{Binary: ffmpeg}).PlayMedia(ctx, fifo, filepath.Join(dir, "sample.mp4")); err != nil {
		t.Fatalf("PlayMedia: %v", err)
	}
	select {
	case err := <-readError:
		t.Fatal(err)
	case data := <-readResult:
		want := []byte{1, 2, 3, 4}
		if string(data) != string(want) {
			t.Fatalf("FIFO received %v, want %v", data, want)
		}
	case <-ctx.Done():
		t.Fatal("timed out reading converted PCM")
	}
}

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

func TestInactivityTimeoutHangsUpAndResets(t *testing.T) {
	svc, _ := newTestService(t)
	svc.InactivityTimeout = 20 * time.Millisecond
	svc.InactivityHangupDelay = 10 * time.Millisecond
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	svc.client = bridge.NewClient(server)
	sess := &session{CallID: "idle-call", State: "pin"}
	svc.mu.Lock()
	svc.sessions[sess.CallID] = sess
	svc.armInactivityTimeoutLocked(sess)
	firstGeneration := sess.inactivityGeneration
	time.Sleep(5 * time.Millisecond)
	svc.armInactivityTimeoutLocked(sess)
	secondGeneration := sess.inactivityGeneration
	svc.mu.Unlock()
	if secondGeneration <= firstGeneration {
		t.Fatalf("timer generation did not advance on activity: first=%d second=%d", firstGeneration, secondGeneration)
	}

	client.SetReadDeadline(time.Now().Add(time.Second))
	message, err := bridge.Decode(bufio.NewReader(client))
	if err != nil {
		t.Fatalf("Decode timeout hangup: %v", err)
	}
	if message.Type != "hangup" || message.CallID != sess.CallID {
		t.Fatalf("timeout produced %+v, want hangup for %q", message, sess.CallID)
	}

	svc.mu.Lock()
	sess.Closed = true
	if sess.inactivityCancel != nil {
		sess.inactivityCancel()
		sess.inactivityCancel = nil
	}
	svc.mu.Unlock()
}

func TestInvalidateUserSessionsHangsUpAndRemovesMatchingCalls(t *testing.T) {
	svc, _ := newTestService(t)
	server, peer := net.Pipe()
	defer server.Close()
	defer peer.Close()
	svc.client = bridge.NewClient(server)
	svc.sessions["call-owned"] = &session{CallID: "call-owned", UserID: "user-a", State: "main"}
	svc.sessions["call-other"] = &session{CallID: "call-other", UserID: "user-b", State: "main"}
	message := make(chan bridge.Message, 1)
	decodeErr := make(chan error, 1)
	go func() {
		decoded, err := bridge.Decode(bufio.NewReader(peer))
		if err != nil {
			decodeErr <- err
			return
		}
		message <- decoded
	}()
	svc.InvalidateUserSessions("user-a")
	select {
	case err := <-decodeErr:
		t.Fatal(err)
	case decoded := <-message:
		if decoded.Type != "hangup" || decoded.CallID != "call-owned" || decoded.Code != 603 {
			t.Fatalf("revocation message=%+v", decoded)
		}
	case <-time.After(time.Second):
		t.Fatal("did not receive revocation hangup")
	}
	if _, exists := svc.sessions["call-owned"]; exists {
		t.Fatal("matching call session survived PIN revocation")
	}
	if _, exists := svc.sessions["call-other"]; !exists {
		t.Fatal("another user's call session was revoked")
	}
}

func TestMenuBackUsesIVRNavigationContracts(t *testing.T) {
	svc, _ := newTestService(t)
	cases := []struct {
		from string
		want string
	}{
		{from: "account_menu", want: "accounts"},
		{from: "draft_menu", want: "account_menu"},
		{from: "read", want: "list"},
		{from: "move_menu", want: "read"},
		{from: "more_options", want: "read"},
	}
	for _, tc := range cases {
		sess := &session{State: tc.from, Authenticated: true}
		if err := svc.handleMenu(nil, sess, bridge.Message{Digit: "#"}); err != nil {
			t.Fatalf("state %q: handleMenu: %v", tc.from, err)
		}
		if sess.State != tc.want {
			t.Errorf("state %q backed to %q, want %q", tc.from, sess.State, tc.want)
		}
	}
}

func TestFormalIVRFlowTracksTransitionsAndBack(t *testing.T) {
	svc, _ := newTestService(t)
	sess := &session{CallID: "flow-call", State: "main", Flow: ivr.NewSession("flow-call")}
	sess.Flow.State = ivr.StateMain
	svc.mu.Lock()
	transitionLocked(sess, "accounts")
	transitionLocked(sess, "account_menu")
	if sess.Flow.State != ivr.State("account_menu") {
		svc.mu.Unlock()
		t.Fatalf("formal flow state=%q, want account_menu", sess.Flow.State)
	}
	backUsingFlowLocked(sess, "main")
	firstBack := sess.State
	backUsingFlowLocked(sess, "main")
	secondBack := sess.State
	svc.mu.Unlock()
	if firstBack != "accounts" || secondBack != "main" {
		t.Fatalf("flow back states=%q,%q, want accounts,main", firstBack, secondBack)
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
	svc, db := newTestService(t)
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
	if persisted, err := db.PinLockout(context.Background(), phone); err != nil || persisted.IsZero() {
		t.Fatalf("persistent lockout missing: %v %v", persisted, err)
	}
	svc.pinMu.Lock()
	svc.pinLock[phone] = time.Now().Add(-time.Second)
	svc.pinMu.Unlock()
	if err := db.SetPinLockout(context.Background(), phone, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if !svc.pinLockedUntil(phone).IsZero() {
		t.Fatal("expired lockout did not clear")
	}
	if _, ok := svc.pinLock[phone]; ok {
		t.Fatal("expired lockout entry was not removed")
	}
}

func TestOutgoingCallsMatchRequestIDNotQueueOrder(t *testing.T) {
	svc, _ := newTestService(t)
	svc.pending = map[string]DialRequest{
		"req-a": {RequestID: "req-a", URI: "sip:a@example.com", UserID: "user-a", Text: "a"},
		"req-b": {RequestID: "req-b", URI: "sip:b@example.com", UserID: "user-b", Text: "b"},
	}
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	done := make(chan error, 1)
	go func() {
		done <- svc.startOutgoing(server, bridge.Message{Type: "call_outgoing", CallID: "call-b", RequestID: "req-b"})
	}()
	client.SetReadDeadline(time.Now().Add(5 * time.Second))
	// startOutgoing only writes on an unknown request; the matching request
	// should therefore complete without a hangup command.
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("startOutgoing: %v", err)
		}
	case <-time.After(100 * time.Millisecond):
	}
	svc.mu.Lock()
	sess := svc.sessions["call-b"]
	remaining := len(svc.pending)
	svc.mu.Unlock()
	if sess == nil || sess.UserID != "user-b" || sess.AlertText != "b" {
		t.Fatalf("wrong outgoing request matched: %+v", sess)
	}
	if remaining != 1 {
		t.Fatalf("pending requests=%d, want 1", remaining)
	}
}

func TestExpiredOutgoingRequestSignalsFailure(t *testing.T) {
	svc, _ := newTestService(t)
	done := make(chan bool, 1)
	svc.pending = map[string]DialRequest{
		"req-expired": {RequestID: "req-expired", URI: "sip:unreachable@example.com", Done: done},
	}
	svc.expirePending("req-expired")
	svc.mu.Lock()
	_, present := svc.pending["req-expired"]
	svc.mu.Unlock()
	if present {
		t.Fatal("expired request remained pending")
	}
	select {
	case success := <-done:
		if success {
			t.Fatal("expired request was reported successful")
		}
	default:
		t.Fatal("expired request did not signal failure")
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

func TestSetMaildirReadUpdatesInfoSuffix(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "msg:2,RS")
	if err := os.WriteFile(old, []byte("message"), 0600); err != nil {
		t.Fatal(err)
	}
	readPath, err := setMaildirRead(old, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(readPath, ":2,RS") {
		t.Fatalf("read path=%q", readPath)
	}
	unreadPath, err := setMaildirRead(readPath, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(unreadPath, ":2,R") {
		t.Fatalf("unread path=%q", unreadPath)
	}
	if _, err := os.Stat(unreadPath); err != nil {
		t.Fatalf("renamed message missing: %v", err)
	}
}

func TestQueueLocalReadStateDoesNotRequireRemoteIMAP(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('read-user','read-user','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('read-account','read-user','Work','work@example.com','Work','imap.example','user','sealed','smtp.example','user','sealed','{}','now')`); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "message:2,")
	if err := os.WriteFile(path, []byte("message"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := db.DB.ExecContext(ctx, `INSERT INTO mail_messages(account_id,folder,path,is_read,updated_at) VALUES('read-account','Inbox',?,0,'now')`, path)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	sess := &session{UserID: "read-user", Messages: []store.MailSummary{{ID: id, AccountID: "read-account", Folder: "Inbox", Path: path, Read: false}}}
	newPath, err := svc.queueLocalReadState(sess, sess.Messages[0], true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(newPath, ":2,S") {
		t.Fatalf("queued path=%q", newPath)
	}
	var storedPath string
	var read int
	if err := db.DB.QueryRowContext(ctx, `SELECT path,is_read FROM mail_messages WHERE id=?`, id).Scan(&storedPath, &read); err != nil {
		t.Fatal(err)
	}
	if storedPath != newPath || read != 1 {
		t.Fatalf("stored path=%q read=%d, want %q and 1", storedPath, read, newPath)
	}
}

func TestSaveDraftAppendsRemoteDraftWithAllAttachments(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('draft-user','draft-user','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('draft-account','draft-user','Work','work@example.com','Work','imap.example','user','sealed','smtp.example','user','sealed','{"Drafts":"Drafts"}','now')`); err != nil {
		t.Fatal(err)
	}
	var gotAccount store.Account
	var gotFolder string
	var gotRaw []byte
	svc.DraftAppender = func(_ context.Context, account store.Account, folder string, raw []byte) error {
		gotAccount, gotFolder, gotRaw = account, folder, append([]byte(nil), raw...)
		return nil
	}
	sess := &session{UserID: "draft-user", ActiveAccount: "draft-account", Draft: draft{To: "recipient@example.com", Subject: "subject", Body: "body", Attachments: []mailer.Attachment{{Filename: "one.wav", ContentType: "audio/wav", Data: []byte("one")}, {Filename: "two.mp4", ContentType: "video/mp4", Data: []byte("two")}}}}
	svc.saveDraft(sess)
	if gotAccount.ID != "draft-account" || gotFolder != "Drafts" {
		t.Fatalf("remote draft target account=%q folder=%q", gotAccount.ID, gotFolder)
	}
	for _, want := range []string{"one.wav", "two.mp4", "recipient@example.com"} {
		if !strings.Contains(string(gotRaw), want) {
			t.Fatalf("remote MIME draft does not contain %q: %s", want, gotRaw)
		}
	}
	drafts, err := db.ListDrafts(ctx, "draft-user", "draft-account")
	if err != nil || len(drafts) != 1 || len(drafts[0].Attachments) != 2 {
		t.Fatalf("stored draft round-trip failed: drafts=%+v err=%v", drafts, err)
	}
}
