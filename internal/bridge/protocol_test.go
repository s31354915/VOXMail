package bridge

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
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

func TestSendContextCancellationWhileQueued(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	wrapped := &blockingConn{
		Conn:    client,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	c := NewClient(wrapped)
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- c.SendContext(firstCtx, Message{Type: "first"})
	}()
	select {
	case <-wrapped.started:
	case <-time.After(time.Second):
		t.Fatal("first writer did not reach the blocking write")
	}

	queuedCtx, cancelQueued := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelQueued()
	started := time.Now()
	err := c.SendContext(queuedCtx, Message{Type: "queued"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued send error=%v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("queued send took too long to cancel: %s", elapsed)
	}
	cancelFirst()
	close(wrapped.release)
	if err := <-firstDone; err == nil {
		t.Fatal("blocked first send unexpectedly succeeded after cancellation")
	}
}

func TestDecodeRejectsOversizedFrame(t *testing.T) {
	frame := `{"version":2,"type":"event","reason":"` + strings.Repeat("x", maxFrameBytes) + `"}` + "\n"
	if _, err := Decode(bufio.NewReader(strings.NewReader(frame))); err == nil {
		t.Fatal("expected oversized bridge frame to be rejected")
	}
}

func TestDecodeRejectsNoNewlineFrameAtLimit(t *testing.T) {
	started := time.Now()
	if _, err := Decode(strings.NewReader(strings.Repeat("x", maxFrameBytes))); err == nil {
		t.Fatal("expected unterminated bridge frame to be rejected")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("unterminated frame took too long to reject: %s", elapsed)
	}
}

func TestDecodeRejectsUnboundedNoNewlineFlood(t *testing.T) {
	flood := &noNewlineFloodReader{}
	if _, err := Decode(flood); err == nil {
		t.Fatal("expected an endless no-newline stream to be rejected")
	}
	if flood.reads > 3 {
		t.Fatalf("reader was consumed %d times after the fixed frame limit", flood.reads)
	}
}

func TestEncodeHandlesPartialWriter(t *testing.T) {
	writer := &partialWriter{}
	if err := Encode(writer, Message{Type: "event", Reason: "partial"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(writer.data), "\n") {
		t.Fatalf("encoded frame has no newline: %q", writer.data)
	}
}

func TestSendContextClosesAfterPartialWriteFailure(t *testing.T) {
	conn := &partialFailConn{closed: make(chan struct{})}
	err := NewClient(conn).SendContext(context.Background(), Message{Type: "event"})
	if err == nil {
		t.Fatal("expected partial write failure")
	}
	select {
	case <-conn.closed:
	case <-time.After(time.Second):
		t.Fatal("partial write failure did not close the connection")
	}
	if len(conn.data) != 1 {
		t.Fatalf("partial connection recorded %d bytes, want one", len(conn.data))
	}
}

type partialWriter struct{ data []byte }

func (w *partialWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	w.data = append(w.data, p[0])
	return 1, nil
}

type blockingConn struct {
	net.Conn
	started   chan struct{}
	release   chan struct{}
	startOnce sync.Once
}

func (c *blockingConn) Write(p []byte) (int, error) {
	c.startOnce.Do(func() { close(c.started) })
	<-c.release
	return c.Conn.Write(p)
}

type noNewlineFloodReader struct{ reads int }

func (r *noNewlineFloodReader) Read(p []byte) (int, error) {
	r.reads++
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

type partialFailConn struct {
	data   []byte
	closed chan struct{}
	once   sync.Once
}

func (c *partialFailConn) Read([]byte) (int, error) { return 0, io.EOF }

func (c *partialFailConn) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	c.data = append(c.data, p[0])
	return 1, io.ErrShortWrite
}

func (c *partialFailConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

func (c *partialFailConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *partialFailConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c *partialFailConn) SetDeadline(time.Time) error      { return nil }
func (c *partialFailConn) SetReadDeadline(time.Time) error  { return nil }
func (c *partialFailConn) SetWriteDeadline(time.Time) error { return nil }

func TestDecodeRequiresMessageType(t *testing.T) {
	if _, err := Decode(bufio.NewReader(strings.NewReader(`{"version":2}` + "\n"))); err == nil {
		t.Fatal("expected missing bridge message type to be rejected")
	}
}
