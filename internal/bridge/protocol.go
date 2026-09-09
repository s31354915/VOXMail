package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
)

const ProtocolVersion = 1

type Message struct {
	Version int    `json:"version"`
	Type    string `json:"type"`
	CallID  string `json:"call_id,omitempty"`
	UserID  string `json:"user_id,omitempty"`
	From    string `json:"from,omitempty"`
	Digit   string `json:"digit,omitempty"`
	Phase   string `json:"phase,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Path    string `json:"path,omitempty"`
	TxPath  string `json:"tx_path,omitempty"`
	RxPath  string `json:"rx_path,omitempty"`
	Command string `json:"command,omitempty"`
	Code    int    `json:"code,omitempty"`
	To      string `json:"to,omitempty"`
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
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.Send(Message{Type: "dial", To: uri})
}

func (c *Client) Send(message Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Encode(c.conn, message)
}

func (c *Client) Reader() *bufio.Reader { return bufio.NewReader(c.conn) }
