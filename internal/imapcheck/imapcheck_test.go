package imapcheck

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

func TestCheckNeverGreetingHonorsParentDeadline(t *testing.T) {
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
		_, err := Check(ctx, Config{Host: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port, Security: "starttls", Username: "user", Password: "password"})
		result <- err
	}()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("Check accepted a peer that never sent an IMAP greeting")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Check exceeded its parent cancellation deadline")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("never-greeting IMAP connection was not closed")
	}
}

func TestCheckStalledTLSHonorsParentDeadline(t *testing.T) {
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
		_, err := Check(ctx, Config{Host: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port, Security: "implicit_tls", Username: "user", Password: "password"})
		result <- err
	}()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("Check accepted a TLS peer that never completed its handshake")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stalled TLS check exceeded its parent cancellation deadline")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stalled TLS connection was not closed")
	}
}

func TestCheckStalledListHonorsParentDeadline(t *testing.T) {
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
			if strings.Contains(strings.ToUpper(line), " LIST ") {
				for i := 0; i < 40; i++ {
					fmt.Fprintf(conn, "* LIST () \"/\" \"Folder-%d\"\r\n", i)
				}
				// Omit the tagged completion response. The client must
				// unblock from transport cancellation instead of waiting.
				_, _ = io.Copy(io.Discard, conn)
				return
			}
			fmt.Fprintf(conn, "%s OK completed\r\n", fields[0])
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := Check(ctx, Config{
			Host: "localhost", Port: listener.Addr().(*net.TCPAddr).Port, Security: "implicit_tls", Username: "user", Password: "password",
			TLSConfig: &tls.Config{RootCAs: roots},
		})
		result <- err
	}()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("Check accepted a LIST command with no tagged completion")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stalled LIST check exceeded its parent cancellation deadline")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stalled LIST connection was not closed")
	}
}

func TestCheckDisconnectDuringListReturnsAndCloses(t *testing.T) {
	port, roots, done := startListFixture(t, func(conn net.Conn) {
		for i := 0; i < 33; i++ {
			fmt.Fprintf(conn, "* LIST () \"/\" \"Folder-%d\"\r\n", i)
		}
		_ = conn.Close()
	})
	result := make(chan error, 1)
	go func() {
		_, err := Check(context.Background(), Config{
			Host: "localhost", Port: port, Security: "implicit_tls", Username: "user", Password: "password",
			TLSConfig: &tls.Config{RootCAs: roots},
		})
		result <- err
	}()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("Check accepted a disconnected LIST command")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Check hung after LIST disconnect")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("LIST disconnect fixture did not stop")
	}
}

func TestCheckMalformedListReturnsAndCloses(t *testing.T) {
	port, roots, done := startListFixture(t, func(conn net.Conn) {
		_, _ = fmt.Fprint(conn, "* LIST () \"/\" \"unterminated\r\n")
		_ = conn.Close()
	})
	result := make(chan error, 1)
	go func() {
		_, err := Check(context.Background(), Config{
			Host: "localhost", Port: port, Security: "implicit_tls", Username: "user", Password: "password",
			TLSConfig: &tls.Config{RootCAs: roots},
		})
		result <- err
	}()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("Check accepted a malformed LIST response")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Check hung after malformed LIST response")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("malformed LIST fixture did not stop")
	}
}

func startListFixture(t *testing.T, onList func(net.Conn)) (int, *x509.CertPool, <-chan struct{}) {
	t.Helper()
	certificate, roots := testCertificate(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
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
			if strings.Contains(strings.ToUpper(line), " LIST ") {
				onList(conn)
				return
			}
			fmt.Fprintf(conn, "%s OK completed\r\n", fields[0])
		}
	}()
	t.Cleanup(func() {
		_ = secureListener.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("LIST fixture did not stop")
		}
	})
	return listener.Addr().(*net.TCPAddr).Port, roots, done
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
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    now.Add(-time.Minute), NotAfter: now.Add(time.Hour),
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true,
	}
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
