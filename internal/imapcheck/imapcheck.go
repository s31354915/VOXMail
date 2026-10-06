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
	"time"

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
	// TLSConfig is optional for deployments using a private CA. Its
	// verification settings are preserved and ServerName is filled from Host.
	TLSConfig *tls.Config
}

const (
	// Keep endpoint dialing independent from the complete onboarding budget.
	connectionTimeout = 15 * time.Second
	// go-imap applies this to each protocol command.
	commandTimeout = 45 * time.Second
	// Folder discovery, authentication, and cleanup share this hard upper
	// bound unless the caller supplies an earlier cancellation.
	operationTimeout = 60 * time.Second
	cleanupTimeout   = 2 * time.Second
)

func Check(ctx context.Context, cfg Config) ([]string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	operationCtx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
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
	setupCtx, setupCancel := context.WithTimeout(operationCtx, commandTimeout)
	defer setupCancel()
	dialCtx, dialCancel := context.WithTimeout(operationCtx, connectionTimeout)
	defer dialCancel()
	dialer := &net.Dialer{Timeout: connectionTimeout}
	address := cfg.Address
	if address == "" {
		address = net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	}
	conn, err := dialer.DialContext(dialCtx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("IMAP connection failed: %w", err)
	}
	raw := conn
	stopClose := context.AfterFunc(operationCtx, func() { _ = raw.Close() })
	defer stopClose()
	if err := setIMAPDeadline(raw, setupCtx); err != nil {
		_ = raw.Close()
		return nil, err
	}
	if security == "implicit_tls" {
		tlsConn := tls.Client(conn, cfg.tlsConfig())
		if err := tlsConn.HandshakeContext(setupCtx); err != nil {
			_ = raw.Close()
			return nil, fmt.Errorf("IMAP TLS handshake failed: %w", err)
		}
		conn = tlsConn
	}
	if err := setIMAPDeadline(conn, setupCtx); err != nil {
		_ = raw.Close()
		return nil, err
	}
	c, err := client.New(conn)
	if err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("IMAP protocol failed: %w", err)
	}
	c.Timeout = commandTimeout
	defer boundedLogout(c, raw)
	if security == "starttls" {
		if err := c.StartTLS(cfg.tlsConfig()); err != nil {
			return nil, fmt.Errorf("IMAP STARTTLS failed: %w", err)
		}
	}
	if err := c.Login(cfg.Username, cfg.Password); err != nil {
		return nil, fmt.Errorf("IMAP authentication failed: %w", err)
	}
	ch := make(chan *imap.MailboxInfo, 32)
	// The package uses the imap.MailboxInfo type for List results; importing
	// it directly keeps the returned contract independent of the client.
	var folders []string
	errCh := make(chan error, 1)
	go func() { errCh <- c.List("", "*", ch) }()
	for info := range ch {
		if info != nil && strings.TrimSpace(info.Name) != "" {
			folders = append(folders, info.Name)
		}
	}
	if err := <-errCh; err != nil {
		return nil, fmt.Errorf("IMAP folder discovery failed: %w", err)
	}
	return folders, nil
}

func (cfg Config) tlsConfig() *tls.Config {
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

func setIMAPDeadline(conn net.Conn, ctx context.Context) error {
	if conn == nil {
		return fmt.Errorf("IMAP connection is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline := time.Now().Add(commandTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	return conn.SetDeadline(deadline)
}

// drainResults keeps consuming until the go-imap producer has closed its
// result channel. If the operation is canceled, closing the owned transport
// is required to unblock the library's direct channel send.
func drainResults[T any](ctx context.Context, abort func(), results <-chan T, start func() error, consume func(T) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	producerDone := make(chan error, 1)
	go func() { producerDone <- start() }()
	for {
		select {
		case result, ok := <-results:
			if !ok {
				return <-producerDone
			}
			if err := consume(result); err != nil {
				if abort != nil {
					abort()
				}
				for range results {
				}
				<-producerDone
				return err
			}
		case err := <-producerDone:
			for result := range results {
				if consumeErr := consume(result); consumeErr != nil {
					return consumeErr
				}
			}
			return err
		case <-ctx.Done():
			if abort != nil {
				abort()
			}
			for range results {
			}
			<-producerDone
			return ctx.Err()
		}
	}
}

func boundedLogout(c *client.Client, raw net.Conn) {
	if c != nil {
		c.Timeout = cleanupTimeout
		_ = c.Logout()
	}
	if raw != nil {
		_ = raw.Close()
	}
}
