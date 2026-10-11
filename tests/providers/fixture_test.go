//go:build integration

package providers

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/textproto"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend"
)

func certificate(t *testing.T) (tls.Certificate, *x509.CertPool, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	encoded := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(encoded) {
		t.Fatal("could not install fixture trust root")
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, roots, encoded
}

type smtpDelivery struct {
	Raw        []byte
	Recipients []string
}

func smtpFixture(t *testing.T, mode, username, password string) (int, *x509.CertPool, <-chan smtpDelivery) {
	t.Helper()
	cert, roots, _ := certificate(t)
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	deliveries := make(chan smtpDelivery, 1)
	done := make(chan struct{})
	var mu sync.Mutex
	var active net.Conn
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		mu.Lock()
		active = conn
		mu.Unlock()
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		reader := bufio.NewReader(conn)
		fmt.Fprint(conn, "220 localhost ESMTP\r\n")
		authenticated := username == ""
		var recipients []string
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(line, "EHLO "), strings.HasPrefix(line, "HELO "):
				fmt.Fprint(conn, "250-localhost\r\n250 AUTH PLAIN\r\n")
			case strings.HasPrefix(line, "AUTH PLAIN"):
				fields := strings.Fields(line)
				if len(fields) != 3 {
					return
				}
				payload, err := base64.StdEncoding.DecodeString(fields[2])
				authenticated = err == nil && string(payload) == "\x00"+username+"\x00"+password
				if !authenticated {
					fmt.Fprint(conn, "535 invalid credentials\r\n")
					return
				}
				fmt.Fprint(conn, "235 authenticated\r\n")
			case strings.HasPrefix(line, "MAIL FROM:"):
				if !authenticated {
					fmt.Fprint(conn, "530 authentication required\r\n")
					return
				}
				fmt.Fprint(conn, "250 sender accepted\r\n")
			case strings.HasPrefix(line, "RCPT TO:"):
				recipients = append(recipients, strings.Trim(strings.TrimPrefix(line, "RCPT TO:"), "<>"))
				fmt.Fprint(conn, "250 recipient accepted\r\n")
			case line == "DATA":
				fmt.Fprint(conn, "354 send message\r\n")
				raw, err := textproto.NewReader(reader).ReadDotBytes()
				if err != nil {
					return
				}
				deliveries <- smtpDelivery{Raw: raw, Recipients: append([]string(nil), recipients...)}
				if mode == "data-drop" {
					return // accepted by the fixture, acknowledgment lost
				}
				fmt.Fprint(conn, "250 queued\r\n")
			case line == "QUIT":
				if mode != "quit-drop" {
					fmt.Fprint(conn, "221 closing\r\n")
				}
				return
			default:
				fmt.Fprint(conn, "500 unsupported command\r\n")
				return
			}
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		if active != nil {
			_ = active.Close()
		}
		mu.Unlock()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("SMTP fixture did not stop")
		}
	})
	return listener.Addr().(*net.TCPAddr).Port, roots, deliveries
}

// Change the actual server's UIDVALIDITY without changing the cached client
// identity. All other behavior delegates to the real in-memory IMAP backend.
type epochBackend struct {
	backend.Backend
	epoch atomic.Uint32
}

type disconnectListener struct {
	net.Listener
	armed atomic.Bool
	drops atomic.Uint32
}

func (l *disconnectListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return disconnectConn{Conn: conn, fixture: l}, nil
}

type disconnectConn struct {
	net.Conn
	fixture *disconnectListener
}

func (c disconnectConn) Write(p []byte) (int, error) {
	if c.fixture.armed.Load() && bytes.Contains(bytes.ToUpper(p), []byte(" FETCH (")) {
		c.fixture.drops.Add(1)
		_ = c.Conn.Close()
		return 0, io.EOF
	}
	return c.Conn.Write(p)
}

func (b *epochBackend) Login(info *goimap.ConnInfo, username, password string) (backend.User, error) {
	u, err := b.Backend.Login(info, username, password)
	if err != nil {
		return nil, err
	}
	return epochUser{User: u, epoch: &b.epoch}, nil
}

type epochUser struct {
	backend.User
	epoch *atomic.Uint32
}

func (u epochUser) GetMailbox(name string) (backend.Mailbox, error) {
	m, err := u.User.GetMailbox(name)
	if err != nil {
		return nil, err
	}
	return epochMailbox{Mailbox: m, epoch: u.epoch}, nil
}

func (u epochUser) ListMailboxes(subscribed bool) ([]backend.Mailbox, error) {
	mailboxes, err := u.User.ListMailboxes(subscribed)
	for i, mailbox := range mailboxes {
		mailboxes[i] = epochMailbox{Mailbox: mailbox, epoch: u.epoch}
	}
	return mailboxes, err
}

type epochMailbox struct {
	backend.Mailbox
	epoch *atomic.Uint32
}

// MoveMessages supplies MOVE for this controlled success fixture. The memory
// backend lacks it; these test mailboxes have no preexisting Deleted messages.
// Production mutation/uncertainty behavior remains in the real IMAP client.
func (m epochMailbox) MoveMessages(uid bool, set *goimap.SeqSet, destination string) error {
	if err := m.Mailbox.CopyMessages(uid, set, destination); err != nil {
		return err
	}
	if err := m.Mailbox.UpdateMessagesFlags(uid, set, goimap.AddFlags, []string{goimap.DeletedFlag}); err != nil {
		return err
	}
	return m.Mailbox.Expunge()
}

func (m epochMailbox) Status(items []goimap.StatusItem) (*goimap.MailboxStatus, error) {
	status, err := m.Mailbox.Status(items)
	if err == nil {
		status.UidValidity = m.epoch.Load()
	}
	return status, err
}
