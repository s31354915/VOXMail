package calls

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/voxmail/voxmail/internal/bridge"
	"github.com/voxmail/voxmail/internal/ivr"
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

func TestCallEventDispatcherIgnoresDTMFStartPhase(t *testing.T) {
	svc, _ := newTestService(t)
	flow := ivr.NewSession("phase-call")
	flow.State = ivr.StateMain
	sess := &session{CallID: "phase-call", State: "main", Authenticated: true, Flow: flow}
	svc.sessions["phase-call"] = sess
	d := newCallEventDispatcher(context.Background(), svc, nil)
	defer d.close()
	if err := d.handle(context.Background(), bridge.Message{Type: "dtmf", CallID: "phase-call", Digit: "1", Phase: "start"}); err != nil {
		t.Fatal(err)
	}
	if sess.State != "main" {
		t.Fatalf("start-phase DTMF changed state to %q", sess.State)
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
