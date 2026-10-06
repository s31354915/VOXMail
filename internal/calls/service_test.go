package calls

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/mail"
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
	"github.com/voxmail/voxmail/internal/phone"
	"github.com/voxmail/voxmail/internal/secret"
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

func TestPCMWindowBytesPreservesWholeSamples(t *testing.T) {
	for _, test := range []struct {
		window time.Duration
		want   int64
	}{
		{window: 500 * time.Millisecond, want: 8000},
		{window: 15 * time.Second, want: 240000},
		{window: 1501 * time.Millisecond, want: 24016},
	} {
		if got := pcmWindowBytes(test.window); got != test.want || got%mediaBytesPerSample != 0 {
			t.Errorf("pcmWindowBytes(%s)=%d, want even %d", test.window, got, test.want)
		}
	}
}

func TestServiceTaskShutdownClosesAdmissionBeforeWaiting(t *testing.T) {
	svc := &Service{}
	started := make(chan struct{})
	release := make(chan struct{})
	svc.startTask(func() {
		close(started)
		<-release
	})
	<-started

	svc.StopTasks()
	late := make(chan struct{}, 1)
	svc.startTask(func() { late <- struct{}{} })
	close(release)
	svc.Wait()

	select {
	case <-late:
		t.Fatal("task admitted after shutdown began")
	default:
	}
}

func TestBeginSIPApplyRejectsActiveCallsAndReleasesIdempotently(t *testing.T) {
	svc := &Service{sessions: make(map[string]*session), pending: make(map[string]DialRequest)}
	svc.sessions["call-1"] = &session{CallID: "call-1"}
	if _, err := svc.BeginSIPApply(context.Background()); err == nil {
		t.Fatal("active call did not block SIP apply")
	}
	delete(svc.sessions, "call-1")
	svc.pending["request-1"] = DialRequest{RequestID: "request-1"}
	if _, err := svc.BeginSIPApply(context.Background()); err == nil {
		t.Fatal("pending outgoing call did not block SIP apply")
	}
	delete(svc.pending, "request-1")
	release, err := svc.BeginSIPApply(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if svc.ActiveCalls() != 0 {
		t.Fatalf("active calls while gated=%d", svc.ActiveCalls())
	}
	release()
	release()
	svc.mu.Lock()
	gated := svc.sipApplying
	svc.mu.Unlock()
	if gated {
		t.Fatal("idempotent release left SIP apply gate active")
	}
}

func TestAlertOutcomeProgressionIsOrderedAndTerminal(t *testing.T) {
	svc := &Service{}
	var got []AlertOutcome
	sess := &session{AlertOutcome: func(outcome AlertOutcome) { got = append(got, outcome) }}
	for _, outcome := range []AlertOutcome{AlertAccepted, AlertRinging, AlertAnswered, AlertPlaybackCompleted, AlertFailed, AlertRinging} {
		svc.reportAlertOutcome(sess, outcome)
	}
	want := []AlertOutcome{AlertAccepted, AlertRinging, AlertAnswered, AlertPlaybackCompleted}
	if len(got) != len(want) {
		t.Fatalf("outcomes=%v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("outcomes=%v, want %v", got, want)
		}
	}

	got = nil
	sess = &session{AlertOutcome: func(outcome AlertOutcome) { got = append(got, outcome) }}
	svc.reportAlertOutcome(sess, AlertAccepted)
	svc.reportAlertOutcome(sess, AlertFailed)
	svc.reportAlertOutcome(sess, AlertFailed)
	if want := []AlertOutcome{AlertAccepted, AlertFailed}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("failed outcomes=%v, want %v", got, want)
	}
}

func TestServiceDelayedTaskCancelsWithParent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc := &Service{ctx: ctx}
	fired := make(chan struct{}, 1)
	svc.startDelayedTask(time.Hour, func() { fired <- struct{}{} })
	cancel()
	svc.StopTasks()
	svc.Wait()

	select {
	case <-fired:
		t.Fatal("delayed task fired after parent cancellation")
	default:
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

func TestResolveIMAPEndpointUsesPinnedResolver(t *testing.T) {
	svc := &Service{IMAPEndpointResolver: func(_ context.Context, host string, port int) (string, error) {
		if host != "imap.example.com" || port != 993 {
			t.Fatalf("resolver received host=%q port=%d", host, port)
		}
		return "198.51.100.8:993", nil
	}}
	got, err := svc.resolveIMAPEndpoint(context.Background(), "imap.example.com", 993)
	if err != nil {
		t.Fatal(err)
	}
	if got != "198.51.100.8:993" {
		t.Fatalf("resolved address=%q, want pinned address", got)
	}
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

func TestInvalidateUserSessionsForDeletionCancelsRecordingRemoteAndAlertWork(t *testing.T) {
	svc, _ := newTestService(t)
	svc.pending = make(map[string]DialRequest)
	eventCtx, eventCancel := context.WithCancel(context.Background())
	recordCanceled := make(chan struct{})
	attachmentCanceled := make(chan struct{})
	alertDone := make(chan bool, 1)
	pendingDone := make(chan bool, 1)
	svc.sessions["deleting-call"] = &session{
		CallID:           "deleting-call",
		UserID:           "delete-user",
		State:            "audio_recording",
		AlertDone:        alertDone,
		eventCtx:         eventCtx,
		eventCancel:      eventCancel,
		RecordCancel:     func() { close(recordCanceled) },
		AttachmentCancel: func() { close(attachmentCanceled) },
	}
	svc.pending["pending-alert"] = DialRequest{RequestID: "pending-alert", UserID: "delete-user", Done: pendingDone}

	svc.InvalidateUserSessionsForDeletion("delete-user")
	select {
	case <-eventCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("remote/event work was not canceled")
	}
	select {
	case <-recordCanceled:
	case <-time.After(time.Second):
		t.Fatal("recording was not canceled")
	}
	select {
	case <-attachmentCanceled:
	case <-time.After(time.Second):
		t.Fatal("attachment playback was not canceled")
	}
	select {
	case success := <-alertDone:
		if success {
			t.Fatal("deleted alert call was reported successful")
		}
	case <-time.After(time.Second):
		t.Fatal("active alert call was not completed as failed")
	}
	select {
	case success := <-pendingDone:
		if success {
			t.Fatal("pending deleted-user alert was reported successful")
		}
	case <-time.After(time.Second):
		t.Fatal("pending alert was not canceled")
	}
	if len(svc.sessions) != 0 || len(svc.pending) != 0 {
		t.Fatalf("deleted-user work survived: sessions=%d pending=%d", len(svc.sessions), len(svc.pending))
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

func TestFormalIVRFlowStateIsAuthoritative(t *testing.T) {
	svc, _ := newTestService(t)
	sess := &session{CallID: "authoritative-flow", State: "main", Flow: ivr.NewSession("authoritative-flow")}
	sess.Flow.State = ivr.State("accounts")
	svc.mu.Lock()
	if got := stateLocked(sess); got != ivr.State("accounts") {
		svc.mu.Unlock()
		t.Fatalf("state helper returned %q, want accounts", got)
	}
	if sess.State != "accounts" {
		svc.mu.Unlock()
		t.Fatalf("legacy state mirror=%q, want accounts", sess.State)
	}
	sess.State = "stale-legacy-value"
	if got := stateLocked(sess); got != ivr.State("accounts") {
		svc.mu.Unlock()
		t.Fatalf("legacy state overrode formal flow: got %q", got)
	}
	transitionLocked(sess, "account_menu")
	if sess.Flow.State != ivr.State("account_menu") || sess.State != "account_menu" {
		svc.mu.Unlock()
		t.Fatalf("transition diverged: flow=%q mirror=%q", sess.Flow.State, sess.State)
	}
	svc.mu.Unlock()
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

func TestDuplicateOutgoingEventsAreIdempotent(t *testing.T) {
	svc, _ := newTestService(t)
	ctx, cancel := context.WithCancel(context.Background())
	svc.ctx = ctx
	defer func() {
		cancel()
		svc.StopTasks()
		svc.Wait()
	}()
	svc.pending = map[string]DialRequest{
		"req-a": {RequestID: "req-a", URI: "sip:a@example.com", UserID: "user-a", Text: "a"},
		"req-b": {RequestID: "req-b", URI: "sip:b@example.com", UserID: "user-b", Text: "b"},
	}
	message := bridge.Message{Type: "call_outgoing", CallID: "call-a", RequestID: "req-a"}
	if err := svc.startOutgoing(nil, message); err != nil {
		t.Fatalf("first call_outgoing: %v", err)
	}
	if err := svc.startOutgoing(nil, bridge.Message{Type: "call_outgoing", CallID: "call-a", RequestID: "req-b"}); err != nil {
		t.Fatalf("duplicate call_outgoing: %v", err)
	}
	svc.mu.Lock()
	sess := svc.sessions["call-a"]
	_, pendingOther := svc.pending["req-b"]
	remaining := len(svc.pending)
	svc.mu.Unlock()
	if sess == nil || sess.UserID != "user-a" || sess.AlertText != "a" {
		t.Fatalf("duplicate replaced the original session: %+v", sess)
	}
	if !pendingOther || remaining != 1 {
		t.Fatalf("duplicate consumed another pending request: pending=%d present=%v", remaining, pendingOther)
	}
}

func TestLateOutgoingEventIsRejectedWithoutCreatingSession(t *testing.T) {
	svc, _ := newTestService(t)
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	done := make(chan error, 1)
	go func() {
		done <- svc.startOutgoing(server, bridge.Message{Type: "call_outgoing", CallID: "late-call", RequestID: "expired-request"})
	}()
	client.SetReadDeadline(time.Now().Add(time.Second))
	message, err := bridge.Decode(bufio.NewReader(client))
	if err != nil {
		t.Fatalf("decode late-event rejection: %v", err)
	}
	if message.Type != "hangup" || message.CallID != "late-call" || message.Code != 603 {
		t.Fatalf("late-event rejection=%+v", message)
	}
	if err := <-done; err != nil {
		t.Fatalf("late-event rejection write: %v", err)
	}
	svc.mu.Lock()
	_, exists := svc.sessions["late-call"]
	svc.mu.Unlock()
	if exists {
		t.Fatal("late outgoing event created an unknown session")
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
		if got := phone.Normalize(input); got != want {
			t.Errorf("phone.Normalize(%q) = %q, want %q", input, got, want)
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
	remoteAppends := 0
	svc.DraftAppender = func(_ context.Context, account store.Account, folder string, raw []byte) error {
		remoteAppends++
		gotAccount, gotFolder, gotRaw = account, folder, append([]byte(nil), raw...)
		return nil
	}
	sess := &session{UserID: "draft-user", ActiveAccount: "draft-account", Draft: draft{To: "recipient@example.com", Subject: "subject", Body: "body", Attachments: []mailer.Attachment{{Filename: "one.wav", ContentType: "audio/wav", Data: []byte("one")}, {Filename: "two.mp4", ContentType: "video/mp4", Data: []byte("two")}}}}
	svc.saveDraft(sess)
	if sess.Draft.ID == "" {
		t.Fatal("first save did not assign a durable draft ID")
	}
	if remoteAppends != 1 {
		t.Fatalf("remote appends after first save=%d, want 1", remoteAppends)
	}
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
	sess.Draft.Body = "edited locally"
	sess.Draft.Attachments = []mailer.Attachment{{Filename: "updated.txt", ContentType: "text/plain", Data: []byte("updated")}}
	svc.saveDraft(sess)
	if remoteAppends != 1 {
		t.Fatalf("remote appends after repeated save=%d, want exactly one", remoteAppends)
	}
	drafts, err = db.ListDrafts(ctx, "draft-user", "draft-account")
	if err != nil || len(drafts) != 1 || drafts[0].Body != "edited locally" || len(drafts[0].Attachments) != 1 || drafts[0].Attachments[0].Filename != "updated.txt" {
		t.Fatalf("repeated local draft save was not canonical: drafts=%+v err=%v", drafts, err)
	}
}

func TestSaveDraftDoesNotRetryFailedRemoteAppend(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('draft-failure-user','draft-failure-user','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('draft-failure-account','draft-failure-user','Work','work@example.com','Work','imap.example','user','sealed','smtp.example','user','sealed','{"Drafts":"Drafts"}','now')`); err != nil {
		t.Fatal(err)
	}
	remoteAppends := 0
	svc.DraftAppender = func(context.Context, store.Account, string, []byte) error {
		remoteAppends++
		return errors.New("remote unavailable")
	}
	sess := &session{UserID: "draft-failure-user", ActiveAccount: "draft-failure-account", Draft: draft{To: "recipient@example.com", Body: "first"}}
	svc.saveDraft(sess)
	if sess.Draft.ID == "" {
		t.Fatal("failed remote append did not preserve the local draft ID")
	}
	sess.Draft.Body = "second"
	svc.saveDraft(sess)
	if remoteAppends != 1 {
		t.Fatalf("failed remote append was retried %d times, want exactly once", remoteAppends)
	}
	draftRecord, err := db.LoadDraft(ctx, sess.UserID, sess.Draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if draftRecord.Body != "second" {
		t.Fatalf("local draft body=%q, want latest local edit", draftRecord.Body)
	}
}

func TestSendDraftJournalsStableMessageIDBeforeSMTP(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()
	box, err := secret.New("test-key-with-more-than-32-characters-123456")
	if err != nil {
		t.Fatal(err)
	}
	svc.Secrets = box
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('send-user','send-user','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveAccount(ctx, box, store.Account{
		ID: "send-account", UserID: "send-user", CanonicalName: "Work", Email: "sender@example.com", SenderName: "Sender",
		IMAPHost: "imap.example", IMAPPort: 993, IMAPUser: "sender", IMAPPassword: "imap-password",
		SMTPHost: "smtp.example", SMTPPort: 465, SMTPUser: "sender", SMTPPassword: "smtp-password", FolderMap: `{}`, AlertFolders: `[]`,
	}); err != nil {
		t.Fatal(err)
	}
	var sentRaw []byte
	svc.SubmitMail = func(config mailer.Config, _ []string, raw []byte) mailer.SendResult {
		if config.Password != "smtp-password" {
			t.Fatalf("SMTP password=%q, want decrypted password", config.Password)
		}
		sentRaw = append([]byte(nil), raw...)
		return mailer.SendResult{Status: mailer.SendAccepted}
	}
	sess := &session{UserID: "send-user", State: "review", Draft: draft{ID: "draft-send-1", To: "recipient@example.com", Subject: "subject", Body: "body"}}
	svc.sendDraft(sess)
	message, err := mail.ReadMessage(bytes.NewReader(sentRaw))
	if err != nil {
		t.Fatalf("sent MIME=%q: %v", sentRaw, err)
	}
	messageID := message.Header.Get("Message-ID")
	if messageID == "" {
		t.Fatal("sent message has no Message-ID")
	}
	journal, err := db.OutboundSubmission(ctx, messageID)
	if err != nil {
		t.Fatal(err)
	}
	if journal.MessageID != messageID || journal.UserID != sess.UserID || journal.AccountID != "send-account" || journal.DraftID != "draft-send-1" || journal.Status != "accepted" || journal.AcceptedAt == "" {
		t.Fatalf("submission journal=%+v", journal)
	}
}

func TestSaveDraftReplacesAttachmentGenerationWithoutDeletingCurrent(t *testing.T) {
	svc, db := newTestService(t)
	svc.DataRoot = t.TempDir()
	ctx := context.Background()
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('generation-user','generation-user','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('generation-account','generation-user','Work','work@example.com','Work','imap','u','p','smtp','u','p','{}','now')`); err != nil {
		t.Fatal(err)
	}
	sess := &session{UserID: "generation-user", ActiveAccount: "generation-account", Draft: draft{To: "recipient@example.com", Attachments: []mailer.Attachment{{Filename: "old.bin", ContentType: "application/octet-stream", Data: []byte("old")}}}}
	svc.saveDraft(sess)
	if sess.Draft.ID == "" {
		t.Fatal("save did not assign durable draft ID")
	}
	first, err := db.LoadDraft(ctx, sess.UserID, sess.Draft.ID)
	if err != nil || len(first.Attachments) != 1 {
		t.Fatalf("first draft=%+v err=%v", first, err)
	}
	oldPath := first.Attachments[0].Path
	sess.Draft.Attachments = []mailer.Attachment{{Filename: "new.bin", ContentType: "application/octet-stream", Data: []byte("new")}}
	svc.saveDraft(sess)
	second, err := db.LoadDraft(ctx, sess.UserID, sess.Draft.ID)
	if err != nil || len(second.Attachments) != 1 {
		t.Fatalf("second draft=%+v err=%v", second, err)
	}
	if second.Attachments[0].Path == oldPath {
		t.Fatal("replacement reused the old attachment generation")
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old attachment still exists, stat error=%v", err)
	}
	if data, err := os.ReadFile(second.Attachments[0].Path); err != nil || string(data) != "new" {
		t.Fatalf("current attachment data=%q err=%v", data, err)
	}
	storageRoot := filepath.Join(svc.DataRoot, "drafts", draftStorageKey(sess.Draft.ID))
	entries, err := os.ReadDir(storageRoot)
	if err != nil {
		t.Fatal(err)
	}
	directories := 0
	for _, entry := range entries {
		if entry.IsDir() {
			directories++
		}
	}
	if directories != 1 {
		t.Fatalf("attachment generations=%d, want exactly 1", directories)
	}
}
