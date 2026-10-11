package calls

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/voxmail/voxmail/internal/bridge"
	"github.com/voxmail/voxmail/internal/ivr"
	"github.com/voxmail/voxmail/internal/store"
)

func TestCallEventDispatcherPreservesPerCallOrder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc := &Service{}
	d := newCallEventDispatcher(ctx, svc, nil)
	started := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	var seen []string
	d.handleFn = func(_ context.Context, message bridge.Message) error {
		if message.Digit == "1" {
			close(started)
			<-release
		}
		mu.Lock()
		seen = append(seen, message.Digit)
		mu.Unlock()
		return nil
	}

	if err := d.dispatch(bridge.Message{Type: "dtmf", CallID: "call-a", Digit: "1"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first event did not start")
	}
	if err := d.dispatch(bridge.Message{Type: "dtmf", CallID: "call-a", Digit: "2"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	if len(seen) != 0 {
		t.Fatalf("second event ran before first completed: %v", seen)
	}
	mu.Unlock()
	close(release)
	deadline := time.After(time.Second)
	for {
		mu.Lock()
		complete := len(seen) == 2
		mu.Unlock()
		if complete {
			break
		}
		select {
		case <-deadline:
			t.Fatal("ordered events did not complete")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	mu.Lock()
	if seen[0] != "1" || seen[1] != "2" {
		t.Fatalf("event order=%v, want [1 2]", seen)
	}
	mu.Unlock()
	d.close()
}

func TestCallEventDispatcherProcessesKeyStartAndIgnoresRelease(t *testing.T) {
	svc, _ := newTestService(t)
	svc.InactivityTimeout = time.Hour
	flow := ivr.NewSession("phase-call")
	flow.State = ivr.StateMain
	sess := &session{CallID: "phase-call", State: "main", Authenticated: true, Flow: flow}
	svc.sessions["phase-call"] = sess
	d := newCallEventDispatcher(context.Background(), svc, nil)
	defer d.close()
	if err := d.handle(context.Background(), bridge.Message{Type: "dtmf", CallID: "phase-call", Digit: "2", Phase: "start"}); err != nil {
		t.Fatal(err)
	}
	if sess.State != "settings" {
		t.Fatalf("key-start DTMF state=%q, want settings", sess.State)
	}
	if err := d.handle(context.Background(), bridge.Message{Type: "dtmf", CallID: "phase-call", Digit: "\x04", Phase: "end"}); err != nil {
		t.Fatal(err)
	}
	if sess.State != "settings" {
		t.Fatalf("release-phase DTMF changed state to %q", sess.State)
	}
	if sess.inactivityCancel != nil {
		sess.inactivityCancel()
	}
}

func TestCallEventDispatcherInterruptsPromptBeforeHandlingDTMF(t *testing.T) {
	svc, _ := newTestService(t)
	sess := &session{CallID: "barge-call"}
	svc.sessions[sess.CallID] = sess
	d := newCallEventDispatcher(context.Background(), svc, nil)
	defer d.close()
	promptStarted := make(chan struct{})
	promptCanceled := make(chan struct{})
	keyHandled := make(chan struct{})
	d.handleFn = func(ctx context.Context, message bridge.Message) error {
		if message.Digit == "0" {
			promptCtx, cancel := context.WithCancel(ctx)
			svc.mu.Lock()
			sess.playCancel = cancel
			svc.mu.Unlock()
			close(promptStarted)
			<-promptCtx.Done()
			close(promptCanceled)
			return nil
		}
		if message.Digit == "1" {
			close(keyHandled)
		}
		return nil
	}
	if err := d.dispatch(bridge.Message{Type: "dtmf", CallID: sess.CallID, Digit: "0", Phase: "start"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-promptStarted:
	case <-time.After(time.Second):
		t.Fatal("menu prompt did not start")
	}
	if err := d.dispatch(bridge.Message{Type: "dtmf", CallID: sess.CallID, Digit: "1", Phase: "start"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-promptCanceled:
	case <-time.After(time.Second):
		t.Fatal("DTMF did not interrupt the active prompt")
	}
	select {
	case <-keyHandled:
	case <-time.After(time.Second):
		t.Fatal("DTMF was not processed after interrupting the prompt")
	}
}

func TestCallEventDispatcherDropsQueuedKeyFromPreviousMenuState(t *testing.T) {
	svc, _ := newTestService(t)
	svc.InactivityTimeout = time.Hour
	flow := ivr.NewSession("stale-menu-call")
	flow.State = ivr.StateMain
	sess := &session{CallID: "stale-menu-call", State: "main", Authenticated: true, Flow: flow}
	svc.sessions[sess.CallID] = sess
	d := newCallEventDispatcher(context.Background(), svc, nil)
	defer d.close()
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	barrierReached := make(chan struct{})
	var mu sync.Mutex
	var handled []string
	d.handleFn = func(_ context.Context, message bridge.Message) error {
		switch message.Type {
		case "barrier":
			close(barrierReached)
		case "dtmf":
			if message.Digit == "1" {
				close(firstStarted)
				<-releaseFirst
				svc.mu.Lock()
				transitionLocked(sess, "settings")
				svc.mu.Unlock()
			}
			mu.Lock()
			handled = append(handled, message.Digit)
			mu.Unlock()
		}
		return nil
	}
	if err := d.dispatch(bridge.Message{Type: "dtmf", CallID: sess.CallID, Digit: "1", Phase: "start"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first menu key did not start")
	}
	if err := d.dispatch(bridge.Message{Type: "dtmf", CallID: sess.CallID, Digit: "2", Phase: "start"}); err != nil {
		t.Fatal(err)
	}
	if err := d.dispatch(bridge.Message{Type: "barrier", CallID: sess.CallID}); err != nil {
		t.Fatal(err)
	}
	close(releaseFirst)
	select {
	case <-barrierReached:
	case <-time.After(time.Second):
		t.Fatal("event worker did not reach the queue barrier")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(handled) != 1 || handled[0] != "1" {
		t.Fatalf("handled queued keys %v, want only the key that changed the menu", handled)
	}
}

func TestCallEventDispatcherIgnoresReleaseAndMalformedDTMF(t *testing.T) {
	d := newCallEventDispatcher(context.Background(), &Service{}, nil)
	defer d.close()
	handled := make(chan string, 1)
	d.handleFn = func(_ context.Context, message bridge.Message) error {
		handled <- message.Digit
		return nil
	}
	for _, message := range []bridge.Message{
		{Type: "dtmf", CallID: "call", Phase: "end"},
		{Type: "dtmf", CallID: "call", Digit: "\x04", Phase: "end"},
		{Type: "dtmf", CallID: "call", Digit: "\x00", Phase: "start"},
		{Type: "dtmf", CallID: "call", Digit: "12", Phase: "start"},
	} {
		if err := d.dispatch(message); err != nil {
			t.Fatalf("dispatch invalid DTMF %q: %v", message.Digit, err)
		}
	}
	if err := d.dispatch(bridge.Message{Type: "dtmf", CallID: "call", Digit: "1", Phase: "start"}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-handled:
		if got != "1" {
			t.Fatalf("handled digit=%q, want 1", got)
		}
	case <-time.After(time.Second):
		t.Fatal("valid DTMF key was dropped")
	}
	select {
	case got := <-handled:
		t.Fatalf("unexpected additional DTMF reached IVR: %q", got)
	case <-time.After(25 * time.Millisecond):
	}
}

func TestCallEventDispatcherDeduplicatesKeyUntilRelease(t *testing.T) {
	d := newCallEventDispatcher(context.Background(), &Service{}, nil)
	defer d.close()
	handled := make(chan string, 2)
	barrier := make(chan struct{})
	d.handleFn = func(_ context.Context, message bridge.Message) error {
		if message.Type == "barrier" {
			close(barrier)
			return nil
		}
		handled <- message.Digit
		return nil
	}
	for _, message := range []bridge.Message{
		{Type: "dtmf", CallID: "call", Digit: "1", Phase: "start"},
		{Type: "dtmf", CallID: "call", Digit: "1", Phase: "start"},
		{Type: "dtmf", CallID: "call", Phase: "end"},
		{Type: "dtmf", CallID: "call", Digit: "1", Phase: "start"},
		{Type: "barrier", CallID: "call"},
	} {
		if err := d.dispatch(message); err != nil {
			t.Fatalf("dispatch %+v: %v", message, err)
		}
	}
	select {
	case <-barrier:
	case <-time.After(time.Second):
		t.Fatal("DTMF worker did not reach the queue barrier")
	}
	for i := 0; i < 2; i++ {
		select {
		case got := <-handled:
			if got != "1" {
				t.Fatalf("handled key=%q, want 1", got)
			}
		case <-time.After(time.Second):
			t.Fatal("valid repeated key after release was lost")
		}
	}
	select {
	case got := <-handled:
		t.Fatalf("duplicate press reached IVR: %q", got)
	default:
	}
}

func TestCallEventDispatcherAcceptsFirstKeyAcrossConsecutiveMenus(t *testing.T) {
	svc, db := newTestService(t)
	svc.InactivityTimeout = time.Hour
	ctx := context.Background()
	if err := db.CreateUser(ctx, store.User{ID: "menu-user", Username: "menu-user", PasswordHash: "p", PINHash: "p", Role: "user", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('menu-account','menu-user','Work','work@example.test','Work','imap.example.test','mail-user','sealed','smtp.example.test','mail-user','sealed','{}','now')`); err != nil {
		t.Fatal(err)
	}
	flow := ivr.NewSession("menu-call")
	flow.State = ivr.StateMain
	sess := &session{CallID: "menu-call", UserID: "menu-user", State: "main", Authenticated: true, Flow: flow}
	svc.sessions[sess.CallID] = sess
	prompts := make(chan string, 4)
	svc.promptFn = func(_ *session, prompt string) error {
		prompts <- prompt
		return nil
	}
	d := newCallEventDispatcher(context.Background(), svc, nil)
	defer d.close()
	waitForState := func(want string) {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			svc.mu.Lock()
			got := string(stateLocked(sess))
			svc.mu.Unlock()
			if got == want {
				return
			}
			time.Sleep(time.Millisecond)
		}
		svc.mu.Lock()
		got := string(stateLocked(sess))
		svc.mu.Unlock()
		t.Fatalf("menu state=%q, want %q", got, want)
	}
	if err := d.dispatch(bridge.Message{Type: "dtmf", CallID: sess.CallID, Digit: "1", Phase: "start"}); err != nil {
		t.Fatal(err)
	}
	waitForState("accounts")
	if err := d.dispatch(bridge.Message{Type: "dtmf", CallID: sess.CallID, Phase: "end"}); err != nil {
		t.Fatal(err)
	}
	if err := d.dispatch(bridge.Message{Type: "dtmf", CallID: sess.CallID, Digit: "\x04", Phase: "start"}); err != nil {
		t.Fatal(err)
	}
	if err := d.dispatch(bridge.Message{Type: "dtmf", CallID: sess.CallID, Digit: "1", Phase: "start"}); err != nil {
		t.Fatal(err)
	}
	waitForState("account_menu")
	for i := 0; i < 2; i++ {
		select {
		case prompt := <-prompts:
			if strings.Contains(prompt, "That key is not available here") {
				t.Fatalf("release sentinel generated an invalid-key prompt: %q", prompt)
			}
		case <-time.After(time.Second):
			t.Fatal("missing response to valid first key")
		}
	}
	d.close()
	select {
	case prompt := <-prompts:
		t.Fatalf("extra menu prompt: %q", prompt)
	default:
	}
}

func TestCallEventDispatcherIsolatesCalls(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := newCallEventDispatcher(ctx, &Service{}, nil)
	started := make(chan struct{})
	release := make(chan struct{})
	callBFinished := make(chan struct{})
	d.handleFn = func(_ context.Context, message bridge.Message) error {
		if message.CallID == "call-a" {
			close(started)
			<-release
		} else if message.CallID == "call-b" {
			close(callBFinished)
		}
		return nil
	}
	if err := d.dispatch(bridge.Message{Type: "dtmf", CallID: "call-a", Digit: "1"}); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := d.dispatch(bridge.Message{Type: "dtmf", CallID: "call-b", Digit: "1"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-callBFinished:
	case <-time.After(time.Second):
		t.Fatal("call B waited for call A")
	}
	close(release)
	d.close()
}

func TestCallEventDispatcherIsolatesStalledProviderAndCancelsIt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := newCallEventDispatcher(ctx, &Service{}, nil)
	providerStarted := make(chan struct{})
	providerCanceled := make(chan struct{})
	callBFinished := make(chan struct{})
	d.handleFn = func(ctx context.Context, message bridge.Message) error {
		if message.CallID == "call-a" {
			close(providerStarted)
			<-ctx.Done()
			close(providerCanceled)
			return ctx.Err()
		}
		if message.CallID == "call-b" {
			close(callBFinished)
		}
		return nil
	}
	if err := d.dispatch(bridge.Message{Type: "dtmf", CallID: "call-a", Digit: "1"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-providerStarted:
	case <-time.After(time.Second):
		t.Fatal("stalled provider operation did not start")
	}
	if err := d.dispatch(bridge.Message{Type: "dtmf", CallID: "call-b", Digit: "1"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-callBFinished:
	case <-time.After(time.Second):
		t.Fatal("call B waited for call A provider operation")
	}
	if err := d.dispatch(bridge.Message{Type: "call_closed", CallID: "call-a"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-providerCanceled:
	case <-time.After(time.Second):
		t.Fatal("call A close did not cancel the stalled provider operation")
	}
	d.close()
}

func TestCallEventDispatcherBoundsDTMFBacklog(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := newCallEventDispatcher(ctx, &Service{}, nil)
	started := make(chan struct{})
	d.handleFn = func(ctx context.Context, message bridge.Message) error {
		if message.Digit == "0" {
			close(started)
			<-ctx.Done()
		}
		return nil
	}
	if err := d.dispatch(bridge.Message{Type: "dtmf", CallID: "call-a", Digit: "0"}); err != nil {
		t.Fatal(err)
	}
	<-started
	begin := time.Now()
	for i := 0; i < callEventQueueSize*8; i++ {
		if err := d.dispatch(bridge.Message{Type: "dtmf", CallID: "call-a", Digit: "1"}); err != nil {
			t.Fatalf("bounded DTMF dispatch: %v", err)
		}
	}
	if elapsed := time.Since(begin); elapsed > 250*time.Millisecond {
		t.Fatalf("DTMF dispatch blocked for %s", elapsed)
	}
	d.close()
}

func TestCallEventDispatcherCancelsWorkerOnClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := newCallEventDispatcher(ctx, &Service{}, nil)
	started := make(chan struct{})
	canceled := make(chan struct{})
	d.handleFn = func(ctx context.Context, _ bridge.Message) error {
		close(started)
		<-ctx.Done()
		close(canceled)
		return ctx.Err()
	}
	if err := d.dispatch(bridge.Message{Type: "dtmf", CallID: "call-a", Digit: "1"}); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := d.dispatch(bridge.Message{Type: "call_closed", CallID: "call-a"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("call close did not cancel worker")
	}
	d.close()
}

func TestRunConnectionStopsWhenContextIsCanceled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- conn
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	svc := &Service{Socket: path}
	done := make(chan error, 1)
	go func() { done <- svc.runConnection(ctx) }()
	select {
	case conn := <-accepted:
		defer conn.Close()
	case <-time.After(time.Second):
		cancel()
		t.Fatal("bridge connection was not accepted")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("runConnection error=%v, want context cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runConnection did not stop after context cancellation")
	}
}
