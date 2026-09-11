package bridge

import (
	"bufio"
	"context"
	"net"
	"testing"
	"time"
)

func TestDialCommand(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	go func() {
		c := NewClient(client)
		_ = c.Dial(context.Background(), "sip:+15551212@sip.example.com")
	}()

	message, err := Decode(bufio.NewReader(server))
	if err != nil {
		t.Fatal(err)
	}
	if message.Type != "dial" {
		t.Fatalf("type %q, want dial", message.Type)
	}
	if message.To != "sip:+15551212@sip.example.com" {
		t.Fatalf("to %q", message.To)
	}
	if message.RequestID == "" {
		t.Fatal("dial command did not include a request ID")
	}
	if message.Version != ProtocolVersion {
		t.Fatalf("version %d", message.Version)
	}
}

func TestDialWithRequestID(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	go func() {
		_ = NewClient(client).DialWithRequestID(context.Background(), "req-1", "sip:x@y")
	}()
	message, err := Decode(bufio.NewReader(server))
	if err != nil {
		t.Fatal(err)
	}
	if message.RequestID != "req-1" || message.To != "sip:x@y" {
		t.Fatalf("unexpected dial message: %+v", message)
	}
}

func TestMessageRoundTrip(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	original := Message{Type: "dial", To: "sip:+15551212@host", CallID: "abc123"}
	go func() {
		_ = NewClient(client).Send(original)
	}()

	message, err := Decode(bufio.NewReader(server))
	if err != nil {
		t.Fatal(err)
	}
	if message.Type != original.Type || message.To != original.To || message.CallID != original.CallID {
		t.Fatalf("round trip mismatch: got %+v want %+v", message, original)
	}
}

func TestDialRespectsContext(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := NewClient(client).Dial(ctx, "sip:x@y"); err == nil {
		t.Fatal("expected cancelled context to abort dial")
	}
}

func TestDialWriteRespectsDeadline(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := NewClient(client).DialWithRequestID(ctx, "req-1", "sip:x@y")
	if err == nil {
		t.Fatal("expected blocked bridge write to time out")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("bridge write took too long to fail: %s", elapsed)
	}
}
