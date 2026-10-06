package bridge

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

const ProtocolVersion = 2

const maxFrameBytes = 64 << 10

type Message struct {
	Version   int    `json:"version"`
	Type      string `json:"type"`
	CallID    string `json:"call_id,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	UserID    string `json:"user_id,omitempty"`
	From      string `json:"from,omitempty"`
	Digit     string `json:"digit,omitempty"`
	Phase     string `json:"phase,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Path      string `json:"path,omitempty"`
	TxPath    string `json:"tx_path,omitempty"`
	RxPath    string `json:"rx_path,omitempty"`
	Command   string `json:"command,omitempty"`
	Code      int    `json:"code,omitempty"`
	To        string `json:"to,omitempty"`
}

func Encode(w io.Writer, message Message) error {
	message.Version = ProtocolVersion
	frame, err := json.Marshal(message)
	if err != nil {
		return err
	}
	frame = append(frame, '\n')
	if len(frame) > maxFrameBytes {
		return fmt.Errorf("bridge frame exceeds %d bytes", maxFrameBytes)
	}
	return writeAll(w, frame)
}

func Decode(r io.Reader) (Message, error) {
	if r == nil {
		return Message{}, io.ErrUnexpectedEOF
	}
	reader := bufio.NewReaderSize(r, maxFrameBytes+1)
	var message Message
	frame := make([]byte, 0, maxFrameBytes)
	for {
		if len(frame) >= maxFrameBytes {
			return Message{}, fmt.Errorf("bridge frame exceeds %d bytes", maxFrameBytes)
		}
		part, err := reader.ReadByte()
		if err == nil {
			frame = append(frame, part)
			if part == '\n' {
				break
			}
			continue
		}
		return Message{}, err
	}
	if err := json.Unmarshal(frame, &message); err != nil {
		return Message{}, err
	}
	if message.Version != ProtocolVersion {
		return Message{}, fmt.Errorf("unsupported bridge protocol version %d", message.Version)
	}
	if message.Type == "" {
		return Message{}, fmt.Errorf("bridge message type is required")
	}
	return message, nil
}

func writeAll(w io.Writer, frame []byte) error {
	for len(frame) > 0 {
		n, err := w.Write(frame)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		frame = frame[n:]
	}
	return nil
}

type Client struct {
	conn      net.Conn
	writeMu   chan struct{}
	writeOnce sync.Once
}

func Dial(path string) (*Client, error) {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return nil, err
	}
	return NewClient(conn), nil
}

// NewClient wraps an existing connection as a bridge client so a long-lived
// connection can be shared for event reception and command sending.
func NewClient(conn net.Conn) *Client { return &Client{conn: conn} }

func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// Dial requests an outgoing call. The remote callee is identified by uri, for
// example "sip:+15551212@sip.example.com". It returns when the request has
// been written, not when the call is answered.
func (c *Client) Dial(ctx context.Context, uri string) error {
	requestID, err := NewRequestID()
	if err != nil {
		return err
	}
	return c.DialWithRequestID(ctx, requestID, uri)
}

// DialWithRequestID asks the shim to place an outgoing call and carry the
// request ID through the call_outgoing event. The ID is what correlates a
// baresip call with the application request; ordering is not sufficient when
// alerts overlap or calls complete out of order.
func (c *Client) DialWithRequestID(ctx context.Context, requestID, uri string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if requestID == "" {
		return fmt.Errorf("request ID is required")
	}
	return c.SendContext(ctx, Message{Type: "dial", RequestID: requestID, To: uri})
}

// NewRequestID returns a short cryptographically random correlation ID.
func NewRequestID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (c *Client) Send(message Message) error {
	return c.SendContext(context.Background(), message)
}

// SendContext writes one complete bridge frame while respecting cancellation
// and deadlines. A disconnected or wedged baresip socket must not block an
// HTTP request or the alert worker indefinitely.
func (c *Client) SendContext(ctx context.Context, message Message) error {
	if c == nil || c.conn == nil {
		return net.ErrClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	frame, err := json.Marshal(func() Message {
		message.Version = ProtocolVersion
		return message
	}())
	if err != nil {
		return err
	}
	frame = append(frame, '\n')
	if len(frame) > maxFrameBytes {
		return fmt.Errorf("bridge frame exceeds %d bytes", maxFrameBytes)
	}

	c.writeOnce.Do(func() { c.writeMu = make(chan struct{}, 1) })
	select {
	case c.writeMu <- struct{}{}:
		defer func() { <-c.writeMu }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Closing the transport is the only portable way to interrupt a blocked
	// net.Conn write when the caller cancels without a deadline. The caller
	// must reconnect after that point; a partially written JSON frame cannot be
	// safely reused.
	stopClose := context.AfterFunc(ctx, func() { _ = c.conn.Close() })
	defer stopClose()
	if deadline, ok := ctx.Deadline(); ok {
		if err := c.conn.SetWriteDeadline(deadline); err != nil {
			return err
		}
		defer c.conn.SetWriteDeadline(time.Time{})
	}
	if err := writeAll(c.conn, frame); err != nil {
		_ = c.conn.Close()
		return err
	}
	return nil
}

func (c *Client) Reader() *bufio.Reader { return bufio.NewReader(c.conn) }
