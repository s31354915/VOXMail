package mailsync

import (
	"bufio"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/voxmail/voxmail/internal/secret"
	"github.com/voxmail/voxmail/internal/store"
)

// idleFakeIMAP answers just enough IMAP for the watcher test: greeting with
// capabilities, LOGIN, SELECT, and the RFC 2177 IDLE exchange. Closing notify
// pushes one untagged EXISTS into the active IDLE session.
type idleFakeIMAP struct {
	listener    net.Listener
	derCert     []byte
	privKey     crypto.Signer
	notify      chan struct{}
	idleStarted chan struct{}
	idleOnce    sync.Once
	logMu       sync.Mutex
	logs        []string
}

func (s *idleFakeIMAP) logf(format string, args ...any) {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	s.logs = append(s.logs, fmt.Sprintf(format, args...))
}

func (s *idleFakeIMAP) dump(t *testing.T) {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	for _, line := range s.logs {
		t.Log("server: " + line)
	}
}

func (s *idleFakeIMAP) tlsConfig() *tls.Config {
	pool := x509.NewCertPool()
	cert, err := x509.ParseCertificate(s.derCert)
	if err != nil {
		return nil
	}
	pool.AddCert(cert)
	return &tls.Config{RootCAs: pool, ServerName: "localhost"}
}

func newIdleFakeIMAP(t *testing.T) *idleFakeIMAP {
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
	server := &idleFakeIMAP{listener: listener, derCert: der, privKey: priv, notify: make(chan struct{}), idleStarted: make(chan struct{})}
	go server.serve()
	t.Cleanup(func() { listener.Close() })
	return server
}

func (s *idleFakeIMAP) serve() {
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
			s.logf("read error: %v", err)
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
			s.idleOnce.Do(func() { close(s.idleStarted) })
			go func() {
				<-s.notify
				write("* 1 EXISTS\r\n")
			}()
			done, _ := reader.ReadString('\n')
			if strings.TrimSpace(strings.ToUpper(done)) != "DONE" {
				return
			}
			write(tag + " OK done\r\n")
		default:
			write(tag + " OK done\r\n")
		}
	}
}

func newIdleWatcherFixture(t *testing.T, server *idleFakeIMAP) (*Service, *store.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "voxmail.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	if err := db.CreateUser(ctx, store.User{ID: "idle-user", Username: "idle-user", PasswordHash: "p", PINHash: "p", Role: "user", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	box, err := secret.New("idle-test-key-with-more-than-32-characters-12")
	if err != nil {
		t.Fatal(err)
	}
	port := server.listener.Addr().(*net.TCPAddr).Port
	account := store.Account{
		ID: "idle-account", UserID: "idle-user", CanonicalName: "Idle Account",
		Email: "user@example.com", SenderName: "User",
		IMAPHost: "localhost", IMAPPort: port, IMAPSecurity: "implicit_tls",
		IMAPUser: "user", IMAPPassword: "imap-password",
		SMTPHost: "smtp.example", SMTPUser: "user", SMTPPassword: "smtp-password", FolderMap: `{}`,
	}
	if err := db.SaveAccount(ctx, box, account); err != nil {
		t.Fatal(err)
	}
	runnerPath := filepath.Join(t.TempDir(), "fake-mbsync")
	if err := os.WriteFile(runnerPath, []byte("#!/bin/sh\nsleep 0.2\n"), 0700); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		Store: db, Root: t.TempDir(), Secrets: box,
		Runner: Runner{Binary: runnerPath},
		Log:    slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	return service, db
}

func TestIdleWatcherTriggersOnNewMail(t *testing.T) {
	server := newIdleFakeIMAP(t)
	service, _ := newIdleWatcherFixture(t, server)

	triggered := make(chan string, 1)
	watcher := newIdleWatcher(service)
	watcher.testTLS = server.tlsConfig()
	watcher.testAllowPrivate = true
	watcher.onIdle = func(ctx context.Context, account store.Account) {
		triggered <- account.ID
	}
	watcher.reconcileEvery = 100 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watcher.reconcileLoop(ctx)

	select {
	case <-server.idleStarted:
	case <-time.After(10 * time.Second):
		server.dump(t)
		t.Fatal("watcher never entered IDLE")
	}
	close(server.notify)
	select {
	case id := <-triggered:
		if id != "idle-account" {
			t.Fatalf("onIdle account = %q, want idle-account", id)
		}
	case <-time.After(10 * time.Second):
		server.dump(t)
		t.Fatal("watcher never surfaced the mailbox update")
	}
}

func TestIdleWatcherRestartsReconfiguredAccount(t *testing.T) {
	server := newIdleFakeIMAP(t)
	service, db := newIdleWatcherFixture(t, server)

	watcher := newIdleWatcher(service)
	watcher.testTLS = server.tlsConfig()
	watcher.testAllowPrivate = true
	watcher.onIdle = func(ctx context.Context, account store.Account) {}
	watcher.reconcileEvery = 100 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watcher.reconcileLoop(ctx)

	deadline := time.Now().Add(10 * time.Second)
	for {
		watcher.mu.Lock()
		count := len(watcher.states)
		watcher.mu.Unlock()
		if count == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("watcher never started the account watcher")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Change the account's IMAP endpoint; the watcher must restart it.
	ctxDB := context.Background()
	box, err := secret.New("idle-test-key-with-more-than-32-characters-12")
	if err != nil {
		t.Fatal(err)
	}
	port := server.listener.Addr().(*net.TCPAddr).Port
	updated := store.Account{
		ID: "idle-account", UserID: "idle-user", CanonicalName: "Idle Account",
		Email: "user@example.com", SenderName: "User",
		IMAPHost: "localhost", IMAPPort: port + 1, IMAPSecurity: "implicit_tls",
		IMAPUser: "user", IMAPPassword: "imap-password",
		SMTPHost: "smtp.example", SMTPUser: "user", SMTPPassword: "smtp-password", FolderMap: `{}`,
	}
	if err := db.SaveAccount(ctxDB, box, updated); err != nil {
		t.Fatal(err)
	}
	// The store bumps config_version on every save, so the expected
	// signature has to come from the stored row, not the local struct.
	stored, err := db.ListAllAccounts(ctxDB)
	if err != nil {
		t.Fatal(err)
	}
	var wantSignature string
	for _, listed := range stored {
		if listed.ID == "idle-account" {
			wantSignature = idleSignature(listed)
		}
	}
	if wantSignature == "" {
		t.Fatal("updated account missing from the listing")
	}
	deadline = time.Now().Add(10 * time.Second)
	for {
		watcher.mu.Lock()
		state, ok := watcher.states["idle-account"]
		count := len(watcher.states)
		watcher.mu.Unlock()
		if ok && state.signature == wantSignature {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("watcher never restarted the reconfigured account; states=%d want=%q got=%v", count, wantSignature, state)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
