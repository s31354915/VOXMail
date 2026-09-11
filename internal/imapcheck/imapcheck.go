// Package imapcheck performs authenticated, TLS-verified IMAP onboarding
// checks. It deliberately returns discovered folders so the web console can
// map roles before mbsync is started.
package imapcheck

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
)

type Config struct {
	Host string
	// Address is an optional already-validated IP:port endpoint. Host remains
	// the TLS SNI and is never used for a second DNS lookup when Address is set.
	Address  string
	Port     int
	Security string
	Username string
	Password string
}

func Check(ctx context.Context, cfg Config) ([]string, error) {
	if strings.TrimSpace(cfg.Host) == "" || cfg.Port < 1 || cfg.Port > 65535 || cfg.Username == "" {
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
	dialer := &net.Dialer{}
	address := cfg.Address
	if address == "" {
		address = net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("IMAP connection failed: %w", err)
	}
	if security == "implicit_tls" {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("IMAP TLS handshake failed: %w", err)
		}
		conn = tlsConn
	}
	c, err := client.New(conn)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("IMAP protocol failed: %w", err)
	}
	defer c.Logout()
	if security == "starttls" {
		if err := c.StartTLS(&tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}); err != nil {
			return nil, fmt.Errorf("IMAP STARTTLS failed: %w", err)
		}
	}
	if err := c.Login(cfg.Username, cfg.Password); err != nil {
		return nil, fmt.Errorf("IMAP authentication failed: %w", err)
	}
	ch := make(chan *imap.MailboxInfo, 32)
	// The package uses the imap.MailboxInfo type for List results; importing
	// it directly keeps the returned contract independent of the client.
	if err := c.List("", "*", ch); err != nil {
		return nil, fmt.Errorf("IMAP folder discovery failed: %w", err)
	}
	var folders []string
	for info := range ch {
		if info != nil && strings.TrimSpace(info.Name) != "" {
			folders = append(folders, info.Name)
		}
	}
	return folders, nil
}
