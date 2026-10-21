package imap

import (
	"bufio"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// idleFakeServer answers just enough IMAP for the Idle path: a greeting with
// capabilities, LOGIN, SELECT, and the RFC 2177 IDLE exchange. Closing the
// server's notify channel pushes one untagged EXISTS into the active IDLE
// session, which is exactly the notification the client must surface.
type idleFakeServer struct {
	listener net.Listener
	derCert  []byte
	privKey  crypto.Signer
	notify   chan struct{}
	// idleStarted closes when the server has answered an IDLE continuation.
	idleStarted chan struct{}
	// idleStartedOnce guards the one close.
	idleStartedOnce sync.Once

	logMu sync.Mutex
	logs  []string
}

func (s *idleFakeServer) logf(format string, args ...any) {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	s.logs = append(s.logs, fmt.Sprintf(format, args...))
}

func (s *idleFakeServer) dump(t *testing.T) {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	for _, line := range s.logs {
		t.Log("server: " + line)
	}
}

func newIdleFakeServer(t *testing.T) *idleFakeServer {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &idleFakeServer{listener: listener, derCert: der, privKey: priv, notify: make(chan struct{}), idleStarted: make(chan struct{})}
	go server.serve()
	t.Cleanup(func() { listener.Close() })
	return server
}

func (s *idleFakeServer) tlsConfig() *tls.Config {
	pool := x509.NewCertPool()
	cert, err := x509.ParseCertificate(s.derCert)
	if err != nil {
		return nil
	}
	pool.AddCert(cert)
	return &tls.Config{RootCAs: pool, ServerName: "localhost"}
}

func (s *idleFakeServer) serve() {
	conn, err := s.listener.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{s.derCert}, PrivateKey: s.privKey}}})
	if err := tlsConn.Handshake(); err != nil {
		s.logf("handshake error: %v", err)
		return
	}
	reader := bufio.NewReader(tlsConn)
	s.logf("connected")
	var mu sync.Mutex
	write := func(line string) {
		mu.Lock()
		defer mu.Unlock()
		s.logf("-> %q", strings.TrimRight(line, "\r\n"))
		_, _ = tlsConn.Write([]byte(line))
	}
	write("* OK [CAPABILITY IMAP4rev1 IDLE] ready\r\n")
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		s.logf("<- %q", line)
		tag, rest, _ := strings.Cut(line, " ")
		verb := strings.ToUpper(rest)
		switch {
		case strings.HasPrefix(verb, "CAPABILITY"):
			write("* CAPABILITY IMAP4rev1 IDLE\r\n" + tag + " OK done\r\n")
		case strings.HasPrefix(verb, "LOGIN"):
			write(tag + " OK done\r\n")
		case strings.HasPrefix(verb, "SELECT"):
			write("* 0 EXISTS\r\n* OK [UIDVALIDITY 1] UIDs valid\r\n" + tag + " OK [READ-WRITE] done\r\n")
		case strings.HasPrefix(verb, "IDLE"):
			write("+ idling\r\n")
			s.signalIdleStarted()
			go s.pushExists(write)
			done, _ := reader.ReadString('\n')
			if strings.TrimSpace(strings.ToUpper(done)) != "DONE" {
				return
			}
			write(tag + " OK done\r\n")
		default:
			write(tag + " BAD unknown command\r\n")
		}
	}
}

func (s *idleFakeServer) signalIdleStarted() {
	s.idleStartedOnce.Do(func() { close(s.idleStarted) })
}

func (s *idleFakeServer) pushExists(write func(string)) {
	<-s.notify
	write("* 1 EXISTS\r\n")
}

func openIdleTestClient(t *testing.T, server *idleFakeServer) *Client {
	t.Helper()
	port := server.listener.Addr().(*net.TCPAddr).Port
	client, err := Open(context.Background(), Config{
		Host:             "localhost",
		Address:          server.listener.Addr().String(),
		Port:             port,
		Security:         "implicit_tls",
		Username:         "user",
		Password:         "secret",
		OperationTimeout: 2 * time.Minute,
		TLSConfig:        server.tlsConfig(),
		AllowPrivate:     true,
	})
	if err != nil {
		server.dump(t)
		t.Fatalf("open idle test client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestIdleSignalsMailboxUpdate(t *testing.T) {
	server := newIdleFakeServer(t)
	client := openIdleTestClient(t, server)
	type outcome struct {
		saw bool
		err error
	}
	results := make(chan outcome, 1)
	go func() {
		saw, err := client.Idle(context.Background(), "INBOX")
		results <- outcome{saw: saw, err: err}
	}()
	select {
	case <-server.idleStarted:
	case <-time.After(10 * time.Second):
		server.dump(t)
		t.Fatal("server never entered IDLE")
	}
	close(server.notify)
	select {
	case got := <-results:
		if !got.saw {
			t.Fatalf("Idle saw=%v err=%v, want the mailbox update surfaced", got.saw, got.err)
		}
		if got.err != nil {
			t.Fatalf("Idle err=%v, want nil after a mailbox update", got.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Idle never returned after the EXISTS notification")
	}
}

func TestIdleEndsOnContextDeadlineWithoutUpdate(t *testing.T) {
	server := newIdleFakeServer(t)
	client := openIdleTestClient(t, server)
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	saw, err := client.Idle(ctx, "INBOX")
	if saw {
		t.Fatal("Idle reported an update where the test sent none")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Idle err=%v, want context deadline exceeded", err)
	}
}
