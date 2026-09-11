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
	return json.NewEncoder(w).Encode(message)
}

func Decode(r *bufio.Reader) (Message, error) {
	var message Message
	if err := json.NewDecoder(r).Decode(&message); err != nil {
		return Message{}, err
	}
	if message.Version != ProtocolVersion {
		return Message{}, fmt.Errorf("unsupported bridge protocol version %d", message.Version)
	}
	return message, nil
}

type Client struct {
	conn net.Conn
	mu   sync.Mutex
}

func Dial(path string) (*Client, error) {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return nil, err
	}
	return &Client{conn: conn}, nil
}

// NewClient wraps an existing connection as a bridge client so a long-lived
// connection can be shared for event reception and command sending.
func NewClient(conn net.Conn) *Client { return &Client{conn: conn} }

func (c *Client) Close() error { return c.conn.Close() }

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

	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok {
		if err := c.conn.SetWriteDeadline(deadline); err != nil {
			return err
		}
		defer c.conn.SetWriteDeadline(time.Time{})
	}
	for len(frame) > 0 {
		n, err := c.conn.Write(frame)
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

func (c *Client) Reader() *bufio.Reader { return bufio.NewReader(c.conn) }
