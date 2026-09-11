package imap

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/backend/memory"
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
}

func hasFlag(flags []string, want string) bool {
	for _, flag := range flags {
		if strings.EqualFold(flag, want) {
			return true
		}
	}
	return false
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
