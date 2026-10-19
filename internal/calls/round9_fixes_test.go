package calls

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/voxmail/voxmail/internal/bridge"
	"github.com/voxmail/voxmail/internal/speech"
	"github.com/voxmail/voxmail/internal/store"
)

func TestRound9GreetSpeaksPinLineWithoutGreetingAsset(t *testing.T) {
	svc := &Service{Media: &PromptPlayer{}}
	say := promptRecorder(svc)
	svc.greet(&session{CallID: "call"})
	got := say()
	if len(got) != 2 {
		t.Fatalf("prompts=%v, want the welcome and then the PIN instruction", got)
	}
	if got[0] != speech.StaticWelcomeText {
		t.Fatalf("first prompt=%q, want %q", got[0], speech.StaticWelcomeText)
	}
	if got[1] != "Enter your PIN, then press pound." {
		t.Fatalf("second prompt=%q, want the PIN instruction", got[1])
	}
}

func TestRound9GreetFailureStillSpeaksPinLine(t *testing.T) {
	svc := &Service{Media: &PromptPlayer{GreetingPath: filepath.Join(t.TempDir(), "missing.wav")}}
	say := promptRecorder(svc)
	svc.greet(&session{CallID: "call"})
	got := say()
	if len(got) != 1 {
		t.Fatalf("prompts=%v, want exactly the PIN instruction and no repeated welcome", got)
	}
	if got[0] != "Enter your PIN, then press pound." {
		t.Fatalf("prompt=%q, want the PIN instruction", got[0])
	}
}

func TestRound9KeyZeroContinuesListFromNext(t *testing.T) {
	svc := &Service{sessions: map[string]*session{}, InactivityTimeout: time.Hour}
	say := promptRecorder(svc)
	dir := t.TempDir()
	first := filepath.Join(dir, "first.eml")
	second := filepath.Join(dir, "second.eml")
	for _, path := range []string{first, second} {
		if err := os.WriteFile(path, []byte("From: a@example.com\r\n\r\nbody"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sess := &session{
		CallID:        "call-0",
		State:         "read",
		Authenticated: true,
		Messages: []store.MailSummary{
			{ID: 1, Path: first, Sender: "one@example.com", Subject: "First"},
			{ID: 2, Path: second, Sender: "two@example.com", Subject: "Second"},
		},
		Cursor: 0,
	}
	svc.sessions["call-0"] = sess
	if err := svc.handleDTMF(nil, bridge.Message{Type: "dtmf", CallID: "call-0", Digit: "0", Phase: "start"}); err != nil {
		t.Fatal(err)
	}
	if sess.State != "list" {
		t.Fatalf("state=%q, want list", sess.State)
	}
	if sess.Cursor != 1 {
		t.Fatalf("cursor=%d, want 1", sess.Cursor)
	}
	got := say()
	if len(got) == 0 || !strings.Contains(got[len(got)-1], "Message 2 of 2") {
		t.Fatalf("prompts=%v, want the list entry for the next message", got)
	}
}

func TestRound9PinStoreErrorDoesNotBurnAttempt(t *testing.T) {
	svc, db := newTestService(t)
	say := promptRecorder(svc)
	svc.InactivityTimeout = time.Hour
	sess := &session{CallID: "call-pin", State: "pin", UserID: "user-1", PIN: "12"}
	svc.sessions["call-pin"] = sess
	db.Close()
	if err := svc.handleDTMF(nil, bridge.Message{Type: "dtmf", CallID: "call-pin", Digit: "#", Phase: "start"}); err != nil {
		t.Fatalf("handleDTMF: %v", err)
	}
	if sess.Failures != 0 {
		t.Fatalf("failures=%d, want 0: a store outage must not count as a wrong PIN", sess.Failures)
	}
	if sess.PIN != "" {
		t.Fatalf("pin=%q, want it cleared for re-entry", sess.PIN)
	}
	got := say()
	if len(got) != 1 || !strings.Contains(got[0], "couldn't check") {
		t.Fatalf("prompts=%v, want a retry notice", got)
	}
}

// bridgeReader collects messages written to one end of a net.Pipe so sends
// never block on an unread pipe.
func bridgeReader(conn net.Conn, out chan<- bridge.Message, stop <-chan struct{}) {
	reader := bufio.NewReader(conn)
	for {
		select {
		case <-stop:
			return
		default:
		}
		_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		message, err := bridge.Decode(reader)
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				continue
			}
			return
		}
		out <- message
	}
}

func TestRound9WorkerErrorHangsUpCallWithoutKillingConnection(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	svc := &Service{sessions: map[string]*session{}, client: bridge.NewClient(client)}
	collected := make(chan bridge.Message, 8)
	stop := make(chan struct{})
	defer close(stop)
	go bridgeReader(server, collected, stop)
	d := newCallEventDispatcher(context.Background(), svc, nil)
	defer d.close()
	var failCallA atomic.Bool
	failCallA.Store(true)
	d.handleFn = func(_ context.Context, message bridge.Message) error {
		if failCallA.Load() && message.CallID == "call-a" {
			return errors.New("handler failed")
		}
		return nil
	}
	if err := d.dispatch(bridge.Message{Type: "dtmf", CallID: "call-a", Digit: "1", Phase: "start"}); err != nil {
		t.Fatal(err)
	}
	select {
	case message := <-collected:
		if message.Type != "hangup" || message.CallID != "call-a" {
			t.Fatalf("message=%+v, want a hangup for call-a", message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no hangup for the call whose handler failed")
	}
	failCallA.Store(false)
	select {
	case err := <-d.errs:
		t.Fatalf("dispatcher reported a fatal error: %v", err)
	default:
	}
	handled := make(chan struct{}, 1)
	d.handleFn = func(context.Context, bridge.Message) error { handled <- struct{}{}; return nil }
	if err := d.dispatch(bridge.Message{Type: "dtmf", CallID: "call-b", Digit: "2", Phase: "start"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-handled:
	case <-time.After(2 * time.Second):
		t.Fatal("the shared connection stopped serving other calls")
	}
}

func TestRound9QueueFullHangsUpCallInsteadOfConnection(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	svc := &Service{sessions: map[string]*session{}, client: bridge.NewClient(client)}
	collected := make(chan bridge.Message, 8)
	stop := make(chan struct{})
	defer close(stop)
	go bridgeReader(server, collected, stop)
	d := newCallEventDispatcher(context.Background(), svc, nil)
	defer d.close()
	release := make(chan struct{})
	d.handleFn = func(context.Context, bridge.Message) error {
		<-release
		return nil
	}
	// Fill the worker: one running job and a full queue.
	for i := 0; i < callEventQueueSize; i++ {
		if err := d.dispatch(bridge.Message{Type: "dtmf", CallID: "call-a", Digit: "1", Phase: ""}); err != nil {
			t.Fatalf("dispatch %d: %v", i, err)
		}
	}
	// One more event saturates the queue, and the next one must hang up only
	// this call instead of tearing down the shared connection.
	if err := d.dispatch(bridge.Message{Type: "call_ringing", CallID: "call-a"}); err != nil {
		t.Fatalf("saturating dispatch: %v", err)
	}
	if err := d.dispatch(bridge.Message{Type: "call_ringing", CallID: "call-a"}); err != nil {
		t.Fatalf("queue-full dispatch: %v", err)
	}
	select {
	case message := <-collected:
		if message.Type != "hangup" || message.CallID != "call-a" {
			t.Fatalf("message=%+v, want a hangup for call-a", message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no hangup for the call with the full queue")
	}
	select {
	case err := <-d.errs:
		t.Fatalf("dispatcher reported a fatal error: %v", err)
	default:
	}
	close(release)
}

func TestRound9WavDurationSeconds(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "short.wav")
	// One second of 16-bit mono 8 kHz PCM.
	var wav bytes.Buffer
	wav.WriteString("RIFF")
	_ = binary.Write(&wav, binary.LittleEndian, uint32(36+16000))
	wav.WriteString("WAVE")
	wav.WriteString("fmt ")
	_ = binary.Write(&wav, binary.LittleEndian, uint32(16))
	_ = binary.Write(&wav, binary.LittleEndian, uint16(1))
	_ = binary.Write(&wav, binary.LittleEndian, uint16(1))
	_ = binary.Write(&wav, binary.LittleEndian, uint32(8000))
	_ = binary.Write(&wav, binary.LittleEndian, uint32(16000))
	_ = binary.Write(&wav, binary.LittleEndian, uint16(2))
	_ = binary.Write(&wav, binary.LittleEndian, uint16(16))
	wav.WriteString("data")
	_ = binary.Write(&wav, binary.LittleEndian, uint32(16000))
	wav.Write(bytes.Repeat([]byte{0x00, 0x01}, 8000))
	if err := os.WriteFile(path, wav.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := wavDurationSeconds(path); math.Abs(got-1) > 1e-6 {
		t.Fatalf("duration=%v, want 1", got)
	}
	junk := filepath.Join(dir, "junk.bin")
	if err := os.WriteFile(junk, []byte("not a wav"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := wavDurationSeconds(junk); got != 0 {
		t.Fatalf("duration=%v, want 0 for unparseable input", got)
	}
}

func TestRound9ParseLoudnormMeasure(t *testing.T) {
	stderr := []byte(`[Parsed_loudnorm_0 @ 0x7f] asetnsamples
{
	"input_i" : "-27.10",
	"input_tp" : "-8.50",
	"input_lra" : "0.00",
	"input_thresh" : "-37.60",
	"output_i" : "-16.00",
	"output_tp" : "-1.50",
	"output_lra" : "0.00",
	"output_thresh" : "-26.60",
	"normalization_type" : "dynamic",
	"target_offset" : "0.30"
}
`)
	measure := parseLoudnormMeasure(stderr)
	if measure == nil || measure.InputI != "-27.10" || measure.InputTP != "-8.50" || measure.TargetOffset != "0.30" {
		t.Fatalf("measure=%+v, want the parsed loudnorm block", measure)
	}
	if _, ok := parseLoudnormFloat("NaN", -70, -5); ok {
		t.Fatal("NaN accepted")
	}
	if _, ok := parseLoudnormFloat("-75", -70, -5); ok {
		t.Fatal("out-of-range value accepted")
	}
}
