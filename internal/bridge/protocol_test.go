package bridge

import (
	"bufio"
	"context"
	"net"
	"testing"
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
	if message.Version != ProtocolVersion {
		t.Fatalf("version %d", message.Version)
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