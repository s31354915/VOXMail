package bridge

import (
	"bufio"
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

func (c *Client) Close() error { return c.conn.Close() }

func (c *Client) Send(message Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Encode(c.conn, message)
}

func (c *Client) Reader() *bufio.Reader { return bufio.NewReader(c.conn) }
