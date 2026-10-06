package imap

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend/memory"
	imapclient "github.com/emersion/go-imap/client"
	"github.com/emersion/go-imap/server"
)

func TestOpenAndMutateAgainstVerifiedTLSIMAPServer(t *testing.T) {
	certificate, roots := testCertificate(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	backend := memory.New()
	imapServer := server.New(backend)
	secureListener := tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{certificate}})
	serverDone := make(chan error, 1)
	go func() { serverDone <- imapServer.Serve(secureListener) }()
	defer func() {
		_ = imapServer.Close()
		select {
		case <-serverDone:
		case <-time.After(2 * time.Second):
			t.Error("IMAP server did not stop")
		}
	}()

	port := listener.Addr().(*net.TCPAddr).Port
	connection, err := Open(context.Background(), Config{
		Host: "localhost", Port: port, Security: "implicit_tls", Username: "username", Password: "password",
		TLSConfig: &tls.Config{RootCAs: roots}, AllowPrivate: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()

	folders, err := connection.ListFolders()
	if err != nil {
		t.Fatal(err)
	}
	if len(folders) != 1 || folders[0] != "INBOX" {
		t.Fatalf("folders = %#v, want [INBOX]", folders)
	}
	identities, uidValidity, err := connection.ListMessageIdentities("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if uidValidity == 0 || len(identities) != 1 || identities[0].UID != 6 {
		t.Fatalf("identities = %#v, UIDVALIDITY=%d", identities, uidValidity)
	}
	if !hasFlag(identities[0].Flags, "\\Seen") {
		t.Fatal("fixture message should initially be seen")
	}
	if err := connection.SetSeenByUID("INBOX", identities[0].UID, uidValidity, false); err != nil {
		t.Fatal(err)
	}
	identities, _, err = connection.ListMessageIdentities("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if hasFlag(identities[0].Flags, "\\Seen") {
		t.Fatal("remote seen flag was not removed")
	}
	if err := connection.SetSeen("INBOX", "<0000000@localhost/>", true); err != nil {
		t.Fatal(err)
	}
	identities, _, err = connection.ListMessageIdentities("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if !hasFlag(identities[0].Flags, "\\Seen") {
		t.Fatal("remote seen flag was not restored by Message-ID")
	}
	if err := connection.SetSeenByUID("INBOX", identities[0].UID, uidValidity+1, false); !errors.Is(err, ErrUIDValidityChanged) {
		t.Fatalf("UIDVALIDITY mismatch error=%v, want ErrUIDValidityChanged", err)
	}
	if err := connection.SetSeen("INBOX", "<absent@example>", true); err == nil {
		t.Fatal("absent Message-ID was accepted")
	}
	duplicate := []byte("Message-ID: <duplicate@example>\r\nSubject: duplicate\r\n\r\nbody\r\n")
	for i := 0; i < 2; i++ {
		if err := connection.Append("INBOX", duplicate); err != nil {
			t.Fatalf("Append duplicate %d: %v", i, err)
		}
	}
	if err := connection.SetSeen("INBOX", "<duplicate@example>", true); !errors.Is(err, ErrAmbiguousMessage) {
		t.Fatalf("duplicate Message-ID error=%v, want ErrAmbiguousMessage", err)
	}
}

func TestDrainResultsAbortsProducerWhenConsumerFails(t *testing.T) {
	consumerErr := errors.New("consumer failed")
	results := make(chan int)
	aborted := make(chan struct{})
	producerErr := errors.New("producer stopped")
	err := drainResults(context.Background(), func() { close(aborted) }, results, func() error {
		results <- 1
		<-aborted
		close(results)
		return producerErr
	}, func(int) error {
		return consumerErr
	})
	if !errors.Is(err, consumerErr) {
		t.Fatalf("drainResults error=%v, want consumer error", err)
	}
}

func TestListResultCountsAcrossChannelBoundary(t *testing.T) {
	for _, count := range []int{0, 1, 32, 33, 128} {
		t.Run(fmt.Sprintf("%d", count), func(t *testing.T) {
			connection, admin := newMemoryIMAPFixture(t)
			for i := 0; i < count-1; i++ {
				if err := admin.Create(fmt.Sprintf("Folder-%03d", i)); err != nil {
					t.Fatal(err)
				}
			}
			folders, err := connection.ListFolders()
			if err != nil {
				t.Fatal(err)
			}
			wantFolders := count
			if wantFolders == 0 {
				wantFolders = 1 // the memory fixture always includes INBOX
			}
			if len(folders) != wantFolders {
				t.Fatalf("folders=%d, want %d for %d extra folders", len(folders), wantFolders, count)
			}

			messageFolder := "INBOX"
			if count == 0 {
				messageFolder = "Empty"
				if err := admin.Create(messageFolder); err != nil {
					t.Fatal(err)
				}
			}
			for i := 1; i < count; i++ {
				raw := fmt.Sprintf("From: sender@example.com\r\nMessage-ID: <%d@example.com>\r\nSubject: fixture\r\n\r\nbody\r\n", i)
				if err := admin.Append(messageFolder, nil, time.Now(), strings.NewReader(raw)); err != nil {
					t.Fatal(err)
				}
			}
			identities, _, err := connection.ListMessageIdentities(messageFolder)
			if err != nil {
				t.Fatal(err)
			}
			wantMessages := count
			if count == 0 {
				wantMessages = 0
			}
			if len(identities) != wantMessages {
				t.Fatalf("messages=%d, want %d for fixture size %d", len(identities), wantMessages, count)
			}
		})
	}
}

func newMemoryIMAPFixture(t *testing.T) (*Client, *imapclient.Client) {
	t.Helper()
	certificate, roots := testCertificate(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	backend := memory.New()
	imapServer := server.New(backend)
	secureListener := tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{certificate}})
	serverDone := make(chan error, 1)
	go func() { serverDone <- imapServer.Serve(secureListener) }()
	t.Cleanup(func() {
		_ = imapServer.Close()
		select {
		case <-serverDone:
		case <-time.After(2 * time.Second):
			t.Error("memory IMAP server did not stop")
		}
	})

	port := listener.Addr().(*net.TCPAddr).Port
	admin, err := imapclient.DialTLS(fmt.Sprintf("localhost:%d", port), &tls.Config{RootCAs: roots, ServerName: "localhost"})
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.Login("username", "password"); err != nil {
		_ = admin.Close()
		t.Fatal(err)
	}
	connection, err := Open(context.Background(), Config{
		Host: "localhost", Port: port, Security: "implicit_tls", Username: "username", Password: "password",
		TLSConfig: &tls.Config{RootCAs: roots}, AllowPrivate: true,
	})
	if err != nil {
		_ = admin.Logout()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = connection.Close()
		_ = admin.Logout()
	})
	return connection, admin
}

func TestReconcileMoveAndSeenStatesAgainstVerifiedTLSIMAPServer(t *testing.T) {
	certificate, roots := testCertificate(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	backend := memory.New()
	imapServer := server.New(backend)
	secureListener := tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{certificate}})
	serverDone := make(chan error, 1)
	go func() { serverDone <- imapServer.Serve(secureListener) }()
	defer func() {
		_ = imapServer.Close()
		select {
		case <-serverDone:
		case <-time.After(2 * time.Second):
			t.Error("IMAP server did not stop")
		}
	}()

	port := listener.Addr().(*net.TCPAddr).Port
	admin, err := imapclient.DialTLS(fmt.Sprintf("localhost:%d", port), &tls.Config{RootCAs: roots, ServerName: "localhost"})
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.Login("username", "password"); err != nil {
		_ = admin.Close()
		t.Fatal(err)
	}
	if err := admin.Create("Archive"); err != nil {
		_ = admin.Logout()
		t.Fatal(err)
	}
	connection, err := Open(context.Background(), Config{
		Host: "localhost", Port: port, Security: "implicit_tls", Username: "username", Password: "password",
		TLSConfig: &tls.Config{RootCAs: roots}, AllowPrivate: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	const messageID = "<0000000@localhost/>"
	state, err := connection.ReconcileSeen("INBOX", messageID, true)
	if err != nil || state != MutationStateApplied {
		t.Fatalf("ReconcileSeen(current state) state=%v err=%v, want applied", state, err)
	}
	state, err = connection.ReconcileSeen("INBOX", messageID, false)
	if err != nil || state != MutationStateNotApplied {
		t.Fatalf("ReconcileSeen(opposite state) state=%v err=%v, want not applied", state, err)
	}
	state, err = connection.ReconcileMove("INBOX", "Archive", messageID)
	if err != nil || state != MutationStateNotApplied {
		t.Fatalf("ReconcileMove(before move) state=%v err=%v, want not applied", state, err)
	}
	if _, err := admin.Select("INBOX", false); err != nil {
		t.Fatal(err)
	}
	moveSet := new(goimap.SeqSet)
	moveSet.AddNum(6)
	if err := admin.UidCopy(moveSet, "Archive"); err != nil {
		t.Fatal(err)
	}
	if err := admin.UidStore(moveSet, goimap.AddFlags, []interface{}{goimap.DeletedFlag}, nil); err != nil {
		t.Fatal(err)
	}
	if err := admin.Expunge(nil); err != nil {
		t.Fatal(err)
	}
	state, err = connection.ReconcileMove("INBOX", "Archive", messageID)
	if err != nil || state != MutationStateApplied {
		t.Fatalf("ReconcileMove(after move) state=%v err=%v, want applied", state, err)
	}
	if err := admin.Logout(); err != nil {
		t.Fatal(err)
	}
}

func TestSeenDisconnectAfterAcceptanceLeavesAnAppliedState(t *testing.T) {
	certificate, roots := testCertificate(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	backend := memory.New()
	imapServer := server.New(backend)
	imapServer.AllowInsecureAuth = true // the wrapper hides the concrete TLS type
	secureListener := tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{certificate}})
	intercepted := &mutationResponseDroppingListener{Listener: secureListener}
	serverDone := make(chan error, 1)
	go func() { serverDone <- imapServer.Serve(intercepted) }()
	defer func() {
		_ = imapServer.Close()
		select {
		case <-serverDone:
		case <-time.After(2 * time.Second):
			t.Error("IMAP server did not stop")
		}
	}()

	port := listener.Addr().(*net.TCPAddr).Port
	admin, err := imapclient.DialTLS(fmt.Sprintf("localhost:%d", port), &tls.Config{RootCAs: roots, ServerName: "localhost"})
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.Login("username", "password"); err != nil {
		_ = admin.Close()
		t.Fatal(err)
	}
	if err := admin.Create("Archive"); err != nil {
		_ = admin.Logout()
		t.Fatal(err)
	}
	if err := admin.Logout(); err != nil {
		t.Fatal(err)
	}

	connection, err := Open(context.Background(), Config{
		Host: "localhost", Port: port, Security: "implicit_tls", Username: "username", Password: "password",
		TLSConfig: &tls.Config{RootCAs: roots}, AllowPrivate: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	const messageID = "<0000000@localhost/>"
	mutationErr := connection.SetSeen("INBOX", messageID, false)
	if !errors.Is(mutationErr, ErrMutationUncertain) {
		_ = connection.Close()
		t.Fatalf("SetSeen error=%v, want ErrMutationUncertain after accepted command", mutationErr)
	}
	_ = connection.Close()

	verification, err := Open(context.Background(), Config{
		Host: "localhost", Port: port, Security: "implicit_tls", Username: "username", Password: "password",
		TLSConfig: &tls.Config{RootCAs: roots}, AllowPrivate: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer verification.Close()
	state, err := verification.ReconcileSeen("INBOX", messageID, false)
	if err != nil || state != MutationStateApplied {
		t.Fatalf("ReconcileSeen after disconnect state=%v err=%v, want applied", state, err)
	}
}

func TestOpenNeverGreetingHonorsParentDeadline(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(io.Discard, conn)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := Open(ctx, Config{Host: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port, Security: "starttls", Username: "user", Password: "password", AllowPrivate: true})
		result <- err
	}()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("Open accepted a peer that never sent an IMAP greeting")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Open exceeded its parent cancellation deadline")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("never-greeting IMAP connection was not closed")
	}
}

func TestCloseBoundsStalledLogout(t *testing.T) {
	certificate, roots := testCertificate(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	secureListener := tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{certificate}})
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := secureListener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		fmt.Fprint(conn, "* OK local IMAP\r\n")
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			if strings.Contains(strings.ToUpper(line), " LOGOUT") {
				// Deliberately do not answer. The client must return on its
				// bounded cleanup deadline and close the transport itself.
				_, _ = io.Copy(io.Discard, conn)
				return
			}
			fmt.Fprintf(conn, "%s OK completed\r\n", fields[0])
		}
	}()
	connection, err := Open(context.Background(), Config{
		Host: "localhost", Port: listener.Addr().(*net.TCPAddr).Port, Security: "implicit_tls", Username: "user", Password: "password",
		TLSConfig: &tls.Config{RootCAs: roots}, AllowPrivate: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_ = connection.Close()
	if elapsed := time.Since(started); elapsed > 4*time.Second {
		t.Fatalf("Close took %s, want bounded cleanup", elapsed)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stalled-logout server did not observe connection close")
	}
}

func TestListMessageIdentitiesStalledFetchHonorsParentCancellation(t *testing.T) {
	certificate, roots := testCertificate(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	secureListener := tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{certificate}})
	done := make(chan struct{})
	commands := make(chan string, 8)
	go func() {
		defer close(done)
		conn, err := secureListener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		fmt.Fprint(conn, "* OK local IMAP\r\n")
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			commands <- strings.TrimRight(line, "\r\n")
			upper := strings.ToUpper(line)
			switch {
			case strings.Contains(upper, "SELECT"), strings.Contains(upper, "EXAMINE"):
				fmt.Fprint(conn, "* FLAGS (\\Seen)\r\n* 1 EXISTS\r\n* OK [UIDVALIDITY 1] ready\r\n")
				fmt.Fprintf(conn, "%s OK [READ-ONLY] SELECT completed\r\n", fields[0])
			case strings.Contains(upper, " FETCH"):
				// Do not send a tagged completion response. The operation
				// must be interrupted by cancellation, not wait forever.
				_, _ = io.Copy(io.Discard, conn)
				return
			default:
				fmt.Fprintf(conn, "%s OK completed\r\n", fields[0])
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	connection, err := Open(ctx, Config{
		Host: "localhost", Port: listener.Addr().(*net.TCPAddr).Port, Security: "implicit_tls", Username: "user", Password: "password",
		TLSConfig: &tls.Config{RootCAs: roots}, AllowPrivate: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	identities, _, err := connection.ListMessageIdentities("INBOX")
	if err == nil {
		var seen []string
		for {
			select {
			case command := <-commands:
				seen = append(seen, command)
			default:
				t.Fatalf("stalled FETCH unexpectedly succeeded: identities=%#v commands=%q", identities, seen)
			}
		}
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("stalled FETCH took %s, want cancellation bound", elapsed)
	}
	_ = connection.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stalled FETCH server did not observe connection close")
	}
}

func TestMutationErrorMarksTransportOutcomeUncertain(t *testing.T) {
	err := mutationError("move IMAP message", io.EOF)
	if !errors.Is(err, ErrMutationUncertain) {
		t.Fatalf("mutationError(EOF)=%v, want ErrMutationUncertain", err)
	}
}

func TestMutationErrorKeepsServerRejectionDefinitive(t *testing.T) {
	serverErr := errors.New("[TRYCREATE] destination does not exist")
	err := mutationError("move IMAP message", serverErr)
	if errors.Is(err, ErrMutationUncertain) {
		t.Fatalf("server rejection was incorrectly marked uncertain: %v", err)
	}
	if !errors.Is(err, serverErr) {
		t.Fatalf("mutationError lost server rejection: %v", err)
	}
}

func hasFlag(flags []string, want string) bool {
	for _, flag := range flags {
		if strings.EqualFold(flag, want) {
			return true
		}
	}
	return false
}

type mutationResponseDroppingListener struct {
	net.Listener
}

func (l *mutationResponseDroppingListener) Accept() (net.Conn, error) {
	connection, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &mutationResponseDroppingConn{Conn: connection}, nil
}

type mutationResponseDroppingConn struct {
	net.Conn
	mu       sync.Mutex
	dropTag  string
	writeBuf []byte
}

func (c *mutationResponseDroppingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.mu.Lock()
		upper := strings.ToUpper(string(p[:n]))
		if index := strings.Index(upper, " UID STORE "); index >= 0 {
			fields := strings.Fields(upper[:index])
			if len(fields) > 0 {
				c.dropTag = fields[len(fields)-1]
			}
		}
		c.mu.Unlock()
	}
	return n, err
}

func (c *mutationResponseDroppingConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dropTag == "" {
		return c.Conn.Write(p)
	}
	c.writeBuf = append(c.writeBuf, p...)
	upper := bytes.ToUpper(c.writeBuf)
	if bytes.Contains(upper, []byte(c.dropTag+" OK ")) {
		_ = c.Conn.Close()
		return len(p), nil
	}
	if len(c.writeBuf) > 4096 {
		c.writeBuf = append([]byte(nil), c.writeBuf[len(c.writeBuf)-1024:]...)
	}
	return len(p), nil
}

func testCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "localhost"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})) {
		t.Fatal("could not create test trust pool")
	}
	return certificate, roots
}
