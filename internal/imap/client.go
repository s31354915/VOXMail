// Package imap contains the small set of authenticated remote mutations that
// mbsync intentionally does not provide as an application action API.
package imap

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net/textproto"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/voxmail/voxmail/internal/netguard"
)

type Config struct {
	Host     string
	Port     int
	Security string
	Username string
	Password string
	// TLSConfig is optional and is intended for deployments using a private
	// CA. Its certificate verification settings are preserved; callers cannot
	// enable plaintext authentication through this field.
	TLSConfig    *tls.Config
	AllowPrivate bool // tests or explicitly isolated single-user deployments
}

type Client struct{ conn *client.Client }

const commandTimeout = 45 * time.Second

type MessageIdentity struct {
	Folder      string
	UID         uint32
	UIDValidity uint32
	MessageID   string
	Flags       []string
}

func Open(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.Port == 0 {
		cfg.Port = 993
	}
	if cfg.Host == "" || cfg.Port < 1 || cfg.Port > 65535 || cfg.Username == "" {
		return nil, fmt.Errorf("incomplete IMAP settings")
	}
	security := strings.ToLower(strings.TrimSpace(cfg.Security))
	if security == "" {
		if cfg.Port == 993 {
			security = "implicit_tls"
		} else {
			security = "starttls"
		}
	}
	if security != "implicit_tls" && security != "starttls" {
		return nil, fmt.Errorf("unsupported IMAP security mode")
	}
	conn, err := netguard.DialContext(ctx, cfg.Host, cfg.Port, "", cfg.AllowPrivate)
	if err != nil {
		return nil, err
	}
	tlsConfig := func() *tls.Config {
		if cfg.TLSConfig == nil {
			return &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}
		}
		clone := cfg.TLSConfig.Clone()
		if clone.ServerName == "" {
			clone.ServerName = cfg.Host
		}
		if clone.MinVersion == 0 {
			clone.MinVersion = tls.VersionTLS12
		}
		return clone
	}
	if security == "implicit_tls" {
		tlsConn := tls.Client(conn, tlsConfig())
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("IMAP TLS handshake: %w", err)
		}
		conn = tlsConn
	}
	c, err := client.New(conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	// Connection setup has its own context, but go-imap otherwise allows a
	// later command to wait indefinitely if a provider stalls.
	c.Timeout = commandTimeout
	if security == "starttls" {
		if err := c.StartTLS(tlsConfig()); err != nil {
			_ = c.Logout()
			return nil, fmt.Errorf("IMAP STARTTLS: %w", err)
		}
	}
	if err := c.Login(cfg.Username, cfg.Password); err != nil {
		_ = c.Logout()
		return nil, fmt.Errorf("IMAP authentication: %w", err)
	}
	return &Client{conn: c}, nil
}

func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Logout()
}

func (c *Client) find(folder, messageID string) (*imap.SeqSet, error) {
	if strings.TrimSpace(folder) == "" || strings.TrimSpace(messageID) == "" {
		return nil, fmt.Errorf("folder and Message-ID are required")
	}
	if _, err := c.conn.Select(folder, false); err != nil {
		return nil, fmt.Errorf("select IMAP folder: %w", err)
	}
	criteria := imap.NewSearchCriteria()
	criteria.Header = textproto.MIMEHeader{}
	criteria.Header.Add("Message-Id", messageID)
	uids, err := c.conn.UidSearch(criteria)
	if err != nil {
		return nil, fmt.Errorf("search IMAP message: %w", err)
	}
	if len(uids) == 0 {
		return nil, fmt.Errorf("message was not found on the IMAP server")
	}
	set := new(imap.SeqSet)
	set.AddNum(uids...)
	return set, nil
}

func (c *Client) SetSeen(folder, messageID string, seen bool) error {
	set, err := c.find(folder, messageID)
	if err != nil {
		return err
	}
	var op imap.StoreItem = imap.RemoveFlags
	if seen {
		op = imap.AddFlags
	}
	if err := c.conn.UidStore(set, op, []interface{}{imap.SeenFlag}, nil); err != nil {
		return fmt.Errorf("update IMAP read state: %w", err)
	}
	return nil
}

func (c *Client) SetSeenByUID(folder string, uid, uidValidity uint32, seen bool) error {
	set, err := c.uidSet(folder, uid, uidValidity)
	if err != nil {
		return err
	}
	var op imap.StoreItem = imap.RemoveFlags
	if seen {
		op = imap.AddFlags
	}
	if err := c.conn.UidStore(set, op, []interface{}{imap.SeenFlag}, nil); err != nil {
		return fmt.Errorf("update IMAP read state: %w", err)
	}
	return nil
}

func (c *Client) Move(folder, destination, messageID string) error {
	set, err := c.find(folder, messageID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(destination) == "" {
		return fmt.Errorf("destination folder is required")
	}
	if err := c.conn.UidMove(set, destination); err != nil {
		return fmt.Errorf("move IMAP message: %w", err)
	}
	return nil
}

func (c *Client) MoveByUID(folder, destination string, uid, uidValidity uint32) error {
	set, err := c.uidSet(folder, uid, uidValidity)
	if err != nil {
		return err
	}
	if strings.TrimSpace(destination) == "" {
		return fmt.Errorf("destination folder is required")
	}
	if err := c.conn.UidMove(set, destination); err != nil {
		return fmt.Errorf("move IMAP message: %w", err)
	}
	return nil
}

func (c *Client) uidSet(folder string, uid, uidValidity uint32) (*imap.SeqSet, error) {
	if c == nil || c.conn == nil || strings.TrimSpace(folder) == "" || uid == 0 {
		return nil, fmt.Errorf("folder and UID are required")
	}
	status, err := c.conn.Select(folder, false)
	if err != nil {
		return nil, fmt.Errorf("select IMAP folder: %w", err)
	}
	if uidValidity != 0 && status.UidValidity != uidValidity {
		return nil, fmt.Errorf("IMAP UIDVALIDITY changed for folder %q", folder)
	}
	set := new(imap.SeqSet)
	set.AddNum(uid)
	return set, nil
}

func (c *Client) Append(folder string, raw []byte) error {
	if c == nil || c.conn == nil || strings.TrimSpace(folder) == "" || len(raw) == 0 {
		return fmt.Errorf("draft folder and message are required")
	}
	if err := c.conn.Append(folder, nil, time.Now(), bytes.NewReader(raw)); err != nil {
		return fmt.Errorf("append IMAP draft: %w", err)
	}
	return nil
}

func (c *Client) ListFolders() ([]string, error) {
	if c == nil || c.conn == nil {
		return nil, fmt.Errorf("IMAP connection is unavailable")
	}
	ch := make(chan *imap.MailboxInfo, 32)
	if err := c.conn.List("", "*", ch); err != nil {
		return nil, err
	}
	var folders []string
	for info := range ch {
		if info == nil || strings.TrimSpace(info.Name) == "" {
			continue
		}
		noselect := false
		for _, attr := range info.Attributes {
			if strings.EqualFold(attr, imap.NoSelectAttr) {
				noselect = true
				break
			}
		}
		if !noselect {
			folders = append(folders, info.Name)
		}
	}
	return folders, nil
}

func (c *Client) ListMessageIdentities(folder string) ([]MessageIdentity, uint32, error) {
	if c == nil || c.conn == nil || strings.TrimSpace(folder) == "" {
		return nil, 0, fmt.Errorf("IMAP folder is required")
	}
	status, err := c.conn.Select(folder, true)
	if err != nil {
		return nil, 0, err
	}
	if status.Messages == 0 {
		return nil, status.UidValidity, nil
	}
	set := new(imap.SeqSet)
	set.AddRange(1, status.Messages)
	items := []imap.FetchItem{imap.FetchUid, imap.FetchFlags, imap.FetchEnvelope}
	ch := make(chan *imap.Message, 32)
	if err := c.conn.Fetch(set, items, ch); err != nil {
		return nil, 0, err
	}
	identities := make([]MessageIdentity, 0, status.Messages)
	for message := range ch {
		if message == nil || message.Uid == 0 {
			continue
		}
		messageID := ""
		if message.Envelope != nil {
			messageID = message.Envelope.MessageId
		}
		identities = append(identities, MessageIdentity{Folder: folder, UID: message.Uid, UIDValidity: status.UidValidity, MessageID: messageID, Flags: append([]string(nil), message.Flags...)})
	}
	return identities, status.UidValidity, nil
}
