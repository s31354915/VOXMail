// Package imap contains the small set of authenticated remote mutations that
// mbsync intentionally does not provide as an application action API.
package imap

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/voxmail/voxmail/internal/netguard"
)

type Config struct {
	Host string
	// Address is an optional already-validated numeric endpoint. When set,
	// dialing never performs another hostname lookup; Host remains the TLS
	// certificate/SNI name.
	Address  string
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

type Client struct {
	conn   *client.Client
	raw    net.Conn
	ctx    context.Context
	stop   func() bool
	cancel context.CancelFunc
	once   sync.Once
	err    error
}

const (
	// Dialing gets its own budget so DNS and unreachable endpoints cannot
	// consume the complete protocol-operation budget.
	connectionTimeout = 15 * time.Second
	// go-imap applies this to each command, including the greeting and logout.
	commandTimeout = 45 * time.Second
	// A single authenticated IMAP workflow cannot keep a connection forever.
	operationTimeout = 60 * time.Second
	// Cleanup is intentionally shorter than a normal command. The transport is
	// closed after this bounded graceful attempt regardless of the result.
	cleanupTimeout = 2 * time.Second
)

var (
	ErrUIDValidityChanged = errors.New("IMAP UIDVALIDITY changed")
	ErrMissingIdentity    = errors.New("IMAP message identity is incomplete")
	ErrAmbiguousMessage   = errors.New("IMAP Message-ID matched more than one message")
	ErrMutationUncertain  = errors.New("IMAP mutation outcome is uncertain")
	ErrMutationNotApplied = errors.New("IMAP mutation was not applied")
)

type MutationState uint8

const (
	MutationStateUnknown MutationState = iota
	MutationStateApplied
	MutationStateNotApplied
)

type MessageIdentity struct {
	Folder      string
	UID         uint32
	UIDValidity uint32
	MessageID   string
	Flags       []string
}

func Open(ctx context.Context, cfg Config) (*Client, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	operationCtx, operationCancel := context.WithTimeout(ctx, operationTimeout)
	setupCtx, setupCancel := context.WithTimeout(operationCtx, commandTimeout)
	defer setupCancel()
	if cfg.Port == 0 {
		cfg.Port = 993
	}
	if cfg.Host == "" || cfg.Port < 1 || cfg.Port > 65535 || cfg.Username == "" {
		operationCancel()
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
		operationCancel()
		return nil, fmt.Errorf("unsupported IMAP security mode")
	}
	dialCtx, dialCancel := context.WithTimeout(operationCtx, connectionTimeout)
	conn, err := netguard.DialContext(dialCtx, cfg.Host, cfg.Port, cfg.Address, cfg.AllowPrivate)
	dialCancel()
	if err != nil {
		operationCancel()
		return nil, err
	}
	raw := conn
	stop := context.AfterFunc(operationCtx, func() { _ = raw.Close() })
	if err := setIMAPDeadline(raw, setupCtx); err != nil {
		stop()
		operationCancel()
		_ = raw.Close()
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
		if err := tlsConn.HandshakeContext(setupCtx); err != nil {
			stop()
			operationCancel()
			_ = raw.Close()
			return nil, fmt.Errorf("IMAP TLS handshake: %w", err)
		}
		conn = tlsConn
	}
	c, err := client.New(conn)
	if err != nil {
		stop()
		operationCancel()
		_ = raw.Close()
		return nil, err
	}
	// Connection setup has its own context, but go-imap otherwise allows a
	// later command to wait indefinitely if a provider stalls.
	c.Timeout = commandTimeout
	if security == "starttls" {
		if err := c.StartTLS(tlsConfig()); err != nil {
			stop()
			operationCancel()
			_ = raw.Close()
			return nil, fmt.Errorf("IMAP STARTTLS: %w", err)
		}
	}
	if err := c.Login(cfg.Username, cfg.Password); err != nil {
		stop()
		operationCancel()
		_ = raw.Close()
		return nil, fmt.Errorf("IMAP authentication: %w", err)
	}
	return &Client{conn: c, raw: raw, ctx: operationCtx, stop: stop, cancel: operationCancel}, nil
}

func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	c.once.Do(func() {
		if c.stop != nil {
			c.stop()
		}
		// Logout is a protocol exchange. Give it a short independent budget,
		// then close the transport so a silent peer cannot hold the caller.
		c.conn.Timeout = cleanupTimeout
		c.err = c.conn.Logout()
		if c.raw != nil {
			_ = c.raw.Close()
		}
		if c.cancel != nil {
			c.cancel()
		}
	})
	return c.err
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

// drainResults owns a go-imap result channel until its producer has returned.
// go-imap sends responses directly into the channel and closes it from the
// command method, so abandoning a consumer can strand the protocol reader.
// Closing the owned transport is the cancellation mechanism supported by the
// v1 client; the drain then lets the producer unwind before returning.
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
			// The library closes results before start returns, but drain it
			// explicitly so buffered results are not silently discarded and
			// this remains safe if that ordering changes.
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

func (c *Client) abortTransport() {
	if c != nil && c.raw != nil {
		_ = c.raw.Close()
	}
}

func (c *Client) operationContext() context.Context {
	if c != nil && c.ctx != nil {
		return c.ctx
	}
	return context.Background()
}

func (c *Client) find(folder, messageID string) (*imap.SeqSet, error) {
	identities, err := c.findMessages(folder, messageID, false)
	if err != nil {
		return nil, err
	}
	if len(identities) == 0 {
		return nil, fmt.Errorf("message was not found on the IMAP server")
	}
	if len(identities) != 1 {
		return nil, fmt.Errorf("%w: %d matches", ErrAmbiguousMessage, len(identities))
	}
	set := new(imap.SeqSet)
	set.AddNum(identities[0].UID)
	return set, nil
}

// findMessages performs an exact Message-ID lookup and fetches the current
// flags for each result. readOnly controls the selected-folder mode. It
// deliberately returns every match so callers can distinguish a unique
// identity from an ambiguous one.
func (c *Client) findMessages(folder, messageID string, readOnly bool) ([]MessageIdentity, error) {
	if c == nil || c.conn == nil || strings.TrimSpace(folder) == "" || strings.TrimSpace(messageID) == "" {
		return nil, fmt.Errorf("folder and Message-ID are required")
	}
	status, err := c.conn.Select(folder, readOnly)
	if err != nil {
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
		return nil, nil
	}
	set := new(imap.SeqSet)
	set.AddNum(uids...)
	items := []imap.FetchItem{imap.FetchUid, imap.FetchFlags, imap.FetchEnvelope}
	ch := make(chan *imap.Message, 32)
	identities := make([]MessageIdentity, 0, len(uids))
	err = drainResults(c.operationContext(), c.abortTransport, ch, func() error {
		return c.conn.UidFetch(set, items, ch)
	}, func(message *imap.Message) error {
		if message == nil || message.Uid == 0 {
			return nil
		}
		foundMessageID := ""
		if message.Envelope != nil {
			foundMessageID = message.Envelope.MessageId
		}
		identities = append(identities, MessageIdentity{
			Folder: folder, UID: message.Uid, UIDValidity: status.UidValidity,
			MessageID: foundMessageID, Flags: append([]string(nil), message.Flags...),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("fetch IMAP message identity: %w", err)
	}
	if len(identities) != len(uids) {
		return nil, fmt.Errorf("fetch IMAP message identity returned %d of %d matches", len(identities), len(uids))
	}
	return identities, nil
}

// ReconcileSeen checks a previously attempted Message-ID flag mutation using
// a new read-only view. It never changes remote state.
func (c *Client) ReconcileSeen(folder, messageID string, seen bool) (MutationState, error) {
	identities, err := c.findMessages(folder, messageID, true)
	if err != nil {
		return MutationStateUnknown, err
	}
	if len(identities) == 0 {
		return MutationStateUnknown, fmt.Errorf("message was not found on the IMAP server")
	}
	if len(identities) != 1 {
		return MutationStateUnknown, fmt.Errorf("%w: %d matches", ErrAmbiguousMessage, len(identities))
	}
	if hasIMAPFlag(identities[0].Flags, imap.SeenFlag) == seen {
		return MutationStateApplied, nil
	}
	return MutationStateNotApplied, nil
}

// ReconcileSeenByUID is the UIDVALIDITY-safe counterpart to ReconcileSeen.
// A missing UID is intentionally unknown: another actor may have moved or
// deleted the message after the uncertain command.
func (c *Client) ReconcileSeenByUID(folder string, uid, uidValidity uint32, seen bool) (MutationState, error) {
	identities, currentValidity, err := c.ListMessageIdentities(folder)
	if err != nil {
		return MutationStateUnknown, err
	}
	if currentValidity != uidValidity {
		return MutationStateUnknown, fmt.Errorf("%w for folder %q", ErrUIDValidityChanged, folder)
	}
	for _, identity := range identities {
		if identity.UID != uid {
			continue
		}
		if hasIMAPFlag(identity.Flags, imap.SeenFlag) == seen {
			return MutationStateApplied, nil
		}
		return MutationStateNotApplied, nil
	}
	return MutationStateUnknown, nil
}

// ReconcileMove confirms a move only when a unique Message-ID is absent from
// the source and present exactly once in the destination, or vice versa. Any
// other observation is deliberately left unknown.
func (c *Client) ReconcileMove(source, destination, messageID string) (MutationState, error) {
	if strings.TrimSpace(source) == "" || strings.TrimSpace(destination) == "" || strings.TrimSpace(messageID) == "" {
		return MutationStateUnknown, fmt.Errorf("source, destination, and Message-ID are required")
	}
	if source == destination {
		return MutationStateUnknown, fmt.Errorf("source and destination folders are identical")
	}
	sourceMatches, err := c.findMessages(source, messageID, true)
	if err != nil {
		return MutationStateUnknown, err
	}
	destinationMatches, err := c.findMessages(destination, messageID, true)
	if err != nil {
		return MutationStateUnknown, err
	}
	if len(sourceMatches) > 1 || len(destinationMatches) > 1 {
		return MutationStateUnknown, fmt.Errorf("%w while reconciling move", ErrAmbiguousMessage)
	}
	switch {
	case len(sourceMatches) == 0 && len(destinationMatches) == 1:
		return MutationStateApplied, nil
	case len(sourceMatches) == 1 && len(destinationMatches) == 0:
		return MutationStateNotApplied, nil
	default:
		return MutationStateUnknown, nil
	}
}

// ReconcileMoveByUID can prove only that an uncertain UID move did not happen
// when the original UID is still present under the same UIDVALIDITY. If it is
// absent, a MOVE may have assigned a new UID in the destination, so the state
// remains unknown without a stable Message-ID.
func (c *Client) ReconcileMoveByUID(source string, uid, uidValidity uint32) (MutationState, error) {
	identities, currentValidity, err := c.ListMessageIdentities(source)
	if err != nil {
		return MutationStateUnknown, err
	}
	if currentValidity != uidValidity {
		return MutationStateUnknown, fmt.Errorf("%w for folder %q", ErrUIDValidityChanged, source)
	}
	for _, identity := range identities {
		if identity.UID == uid {
			return MutationStateNotApplied, nil
		}
	}
	return MutationStateUnknown, nil
}

func hasIMAPFlag(flags []string, want string) bool {
	for _, flag := range flags {
		if strings.EqualFold(flag, want) {
			return true
		}
	}
	return false
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
		return mutationError("update IMAP read state", err)
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
		return mutationError("update IMAP read state", err)
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
		return mutationError("move IMAP message", err)
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
		return mutationError("move IMAP message", err)
	}
	return nil
}

func (c *Client) uidSet(folder string, uid, uidValidity uint32) (*imap.SeqSet, error) {
	if c == nil || c.conn == nil || strings.TrimSpace(folder) == "" || uid == 0 {
		return nil, fmt.Errorf("%w: folder and UID are required", ErrMissingIdentity)
	}
	if uidValidity == 0 {
		return nil, fmt.Errorf("%w: UIDVALIDITY is required for UID %d", ErrMissingIdentity, uid)
	}
	status, err := c.conn.Select(folder, false)
	if err != nil {
		return nil, fmt.Errorf("select IMAP folder: %w", err)
	}
	if uidValidity != 0 && status.UidValidity != uidValidity {
		return nil, fmt.Errorf("%w for folder %q", ErrUIDValidityChanged, folder)
	}
	set := new(imap.SeqSet)
	set.AddNum(uid)
	return set, nil
}

func mutationError(operation string, err error) error {
	if err == nil {
		return nil
	}
	if mutationTransportError(err) {
		return fmt.Errorf("%w: %s: %v", ErrMutationUncertain, operation, err)
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func mutationTransportError(err error) bool {
	var networkErr net.Error
	if errors.As(err, &networkErr) || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "connection closed")
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
	var folders []string
	err := drainResults(c.operationContext(), c.abortTransport, ch, func() error {
		return c.conn.List("", "*", ch)
	}, func(info *imap.MailboxInfo) error {
		if info == nil || strings.TrimSpace(info.Name) == "" {
			return nil
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
		return nil
	})
	if err != nil {
		return nil, err
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
	identities := make([]MessageIdentity, 0, status.Messages)
	err = drainResults(c.operationContext(), c.abortTransport, ch, func() error {
		return c.conn.Fetch(set, items, ch)
	}, func(message *imap.Message) error {
		if message == nil || message.Uid == 0 {
			return nil
		}
		messageID := ""
		if message.Envelope != nil {
			messageID = message.Envelope.MessageId
		}
		identities = append(identities, MessageIdentity{Folder: folder, UID: message.Uid, UIDValidity: status.UidValidity, MessageID: messageID, Flags: append([]string(nil), message.Flags...)})
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	return identities, status.UidValidity, nil
}
