package mailsync

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/backend/memory"
	"github.com/emersion/go-imap/server"
	remoteimap "github.com/voxmail/voxmail/internal/imap"
)

func TestRetainedReadRetryAgainstVerifiedTLSProviderPreservesDistinctCopies(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	backend := memory.New()
	user, err := backend.Login(nil, "username", "password")
	if err != nil {
		t.Fatal(err)
	}
	if err := user.CreateMailbox("Alias"); err != nil {
		t.Fatal(err)
	}
	mailbox, err := user.GetMailbox("Alias")
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("From: sender@example.test\r\nMessage-ID: <same@test>\r\nSubject: copy\r\n\r\nBody\r\n")
	for i := 0; i < 2; i++ {
		if err := mailbox.CreateMessage(nil, time.Now(), bytes.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
	}
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: private}}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	provider := server.New(backend)
	stopped := make(chan error, 1)
	go func() { stopped <- provider.Serve(listener) }()
	defer func() {
		_ = provider.Close()
		select {
		case <-stopped:
		case <-time.After(time.Second):
			t.Error("provider fixture did not stop")
		}
	}()
	config := remoteimap.Config{Host: "localhost", Port: listener.Addr().(*net.TCPAddr).Port, Security: "implicit_tls", Username: "username", Password: "password", TLSConfig: &tls.Config{RootCAs: roots}, AllowPrivate: true}
	observer, err := remoteimap.Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	before, epoch, err := observer.ListMessageIdentities("Alias")
	if err != nil || len(before) != 2 {
		t.Fatalf("provider copies=%v err=%v", before, err)
	}
	db, account, pending := queuedRetainedFixture(t)
	pending[0].UID, pending[0].UIDValidity = before[0].UID, epoch
	if _, err := db.DB.Exec(`UPDATE mail_messages SET imap_uid=?,uid_validity=? WHERE id=1`, before[0].UID, epoch); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`UPDATE message_mutations SET source_uid=?,source_uid_validity=? WHERE message_id=1`, before[0].UID, epoch); err != nil {
		t.Fatal(err)
	}
	open := func() (readMutationConnection, error) { return remoteimap.Open(ctx, config) }
	root := t.TempDir()
	for _, folder := range []string{"OldAlias", "NewAlias"} {
		dir := filepath.Join(root, "mail", "a", folder, "cur")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "message:2,")
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if folder == "OldAlias" {
			if _, err := db.DB.Exec(`UPDATE mail_messages SET path=? WHERE id=1`, path); err != nil {
				t.Fatal(err)
			}
		} else {
			if _, err := db.DB.Exec(`INSERT INTO mail_messages(id,account_id,folder,path,message_id,is_read,imap_uid,uid_validity,updated_at) VALUES(3,'a','NewAlias',?,'<same@test>',0,?,?,'now')`, path, before[0].UID, epoch); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := (&Service{Store: db, Root: root}).confirmRetainedReads(ctx, account, pending, open); err != nil {
		t.Fatal(err)
	}
	after, _, err := observer.ListMessageIdentities("Alias")
	if err != nil || len(after) != 2 {
		t.Fatalf("provider copies=%v err=%v", after, err)
	}
	for _, item := range after {
		seen := false
		for _, flag := range item.Flags {
			seen = seen || strings.EqualFold(flag, `\Seen`)
		}
		if seen != (item.UID == before[0].UID) {
			t.Fatalf("read retry targeted wrong same-Message-ID copy: %+v", item)
		}
	}
	for _, id := range []int64{1, 3} {
		var read bool
		var path string
		if err := db.DB.QueryRow(`SELECT is_read,path FROM mail_messages WHERE id=?`, id).Scan(&read, &path); err != nil || !read || !strings.HasSuffix(path, ":2,S") {
			t.Fatalf("TLS-confirmed cache %d: path=%s read=%v err=%v", id, path, read, err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
	journal, err := db.ListMessageMutations(ctx, "u", "a", 10)
	if err != nil || len(journal) != 1 || journal[0].Status != "synchronized" {
		t.Fatalf("journal=%v err=%v", journal, err)
	}
}
