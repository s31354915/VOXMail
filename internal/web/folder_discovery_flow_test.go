package web

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/backend/memory"
	imapclient "github.com/emersion/go-imap/client"
	"github.com/emersion/go-imap/server"
	"github.com/voxmail/voxmail/internal/folderpolicy"
	"github.com/voxmail/voxmail/internal/imapcheck"
	"github.com/voxmail/voxmail/internal/mailer"
	"github.com/voxmail/voxmail/internal/secret"
)

func TestSavedProviderDiscoveryFeedsOwnerMappingBeforeInitialSync(t *testing.T) {
	srv, h := voiceWebFixture(t)
	box, err := secret.New("discovery-flow-disposable-key-0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	srv.Secrets = box
	// Replace only the preliminary reachability dial. accountAddress still
	// checks the resulting numeric endpoint against the production IP policy.
	srv.accountEndpoint = func(_ context.Context, host string, port int) (string, error) {
		return net.JoinHostPort(host, fmt.Sprint(port)), nil
	}
	certificateSource := httptest.NewTLSServer(http.NotFoundHandler())
	cert := certificateSource.TLS.Certificates[0]
	roots := x509.NewCertPool()
	roots.AddCert(certificateSource.Certificate())
	certificateSource.Close()
	tlsConfig := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	imapListener, err := tls.Listen("tcp", "127.0.0.1:0", tlsConfig)
	if err != nil {
		t.Fatal(err)
	}
	imapServer := server.New(memory.New())
	imapDone := make(chan error, 1)
	go func() { imapDone <- imapServer.Serve(imapListener) }()
	t.Cleanup(func() {
		_ = imapServer.Close()
		select {
		case <-imapDone:
		case <-time.After(2 * time.Second):
			t.Error("IMAP fixture did not stop")
		}
	})
	admin, err := imapclient.DialTLS(imapListener.Addr().String(), &tls.Config{RootCAs: roots})
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.Login("username", "password"); err != nil {
		t.Fatal(err)
	}
	for _, folder := range []string{"[Gmail]/Inbox", "Projects/2026"} {
		if err := admin.Create(folder); err != nil {
			t.Fatal(err)
		}
	}
	_ = admin.Logout()
	smtpListener, err := tls.Listen("tcp", "127.0.0.1:0", tlsConfig)
	if err != nil {
		t.Fatal(err)
	}
	smtpDone := make(chan struct{})
	go func() {
		defer close(smtpDone)
		for {
			connection, err := smtpListener.Accept()
			if err != nil {
				return
			}
			func() {
				defer connection.Close()
				_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
				fmt.Fprint(connection, "220 fixture ESMTP\r\n")
				reader := bufio.NewReader(connection)
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					line = strings.TrimSpace(line)
					if strings.HasPrefix(line, "EHLO ") {
						fmt.Fprint(connection, "250-fixture\r\n250 AUTH PLAIN\r\n")
						continue
					}
					if strings.HasPrefix(line, "AUTH PLAIN ") {
						decoded, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(line, "AUTH PLAIN "))
						if string(decoded) == "\x00username\x00password" {
							fmt.Fprint(connection, "235 authenticated\r\n")
						} else {
							fmt.Fprint(connection, "535 authentication failed\r\n")
						}
						continue
					}
					if line == "QUIT" {
						fmt.Fprint(connection, "221 bye\r\n")
						return
					}
					fmt.Fprint(connection, "500 unsupported fixture command\r\n")
				}
			}()
		}
	}()
	t.Cleanup(func() {
		_ = smtpListener.Close()
		select {
		case <-smtpDone:
		case <-time.After(6 * time.Second):
			t.Error("SMTP fixture did not stop")
		}
	})
	var imapChecks, smtpChecks atomic.Int32
	// Request endpoint policy remains enabled. Only these private fixture
	// adapters route an already-validated public endpoint to disposable TLS
	// servers; the actual authenticated protocol checks still run.
	srv.imapAccountCheck = func(ctx context.Context, cfg imapcheck.Config) ([]string, error) {
		imapChecks.Add(1)
		if cfg.Host != "8.8.8.8" || cfg.Address != "8.8.8.8:993" {
			t.Errorf("IMAP endpoint was not policy-pinned: %q %q", cfg.Host, cfg.Address)
		}
		cfg.Host = "127.0.0.1"
		cfg.Address = imapListener.Addr().String()
		cfg.TLSConfig = &tls.Config{RootCAs: roots}
		return imapcheck.Check(ctx, cfg)
	}
	srv.smtpAccountCheck = func(ctx context.Context, cfg mailer.Config) error {
		smtpChecks.Add(1)
		if cfg.Host != "8.8.8.8" || cfg.Address != "8.8.8.8:465" {
			t.Errorf("SMTP endpoint was not policy-pinned: %q %q", cfg.Host, cfg.Address)
		}
		cfg.Host = "127.0.0.1"
		cfg.Address = smtpListener.Addr().String()
		cfg.TLSConfig = &tls.Config{RootCAs: roots}
		return mailer.Check(ctx, cfg)
	}
	request := map[string]any{"canonical_name": "Personal", "email": "owner@example.test", "sender_name": "Owner", "imap_host": "8.8.8.8", "imap_port": 993, "imap_security": "implicit_tls", "imap_user": "username", "imap_password": "password", "smtp_host": "8.8.8.8", "smtp_port": 465, "smtp_security": "implicit_tls", "smtp_user": "username", "smtp_password": "password", "folder_map": map[string]string{}, "folder_roles": map[string]string{}}
	response := voiceRequest(h, "user", http.MethodPost, "/api/v1/accounts", request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create account: %d %s", response.Code, response.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("created ID: %+v %v", created, err)
	}
	policyPath := "/api/v1/accounts/" + created.ID + "/folder-policy"
	testBody := map[string]string{"account_id": created.ID}
	if response := voiceRequest(h, "admin", http.MethodPost, "/api/v1/accounts/test", testBody); response.Code != http.StatusNotFound {
		t.Fatalf("cross-owner discovery: %d", response.Code)
	}
	if imapChecks.Load() != 0 || smtpChecks.Load() != 0 {
		t.Fatal("cross-owner request contacted providers")
	}
	response = voiceRequest(h, "user", http.MethodPost, "/api/v1/accounts/test", testBody)
	if response.Code != http.StatusOK {
		t.Fatalf("authenticated discovery: %d %s", response.Code, response.Body.String())
	}
	if imapChecks.Load() != 1 || smtpChecks.Load() != 1 {
		t.Fatal("both provider checks did not run")
	}
	var snapshot struct {
		Version int                 `json:"config_version"`
		Folders map[string]string   `json:"folders"`
		Policy  folderpolicy.Policy `json:"policy"`
		Mapping map[string]string   `json:"folder_map"`
		Roles   map[string]string   `json:"folder_roles"`
	}
	response = voiceRequest(h, "user", http.MethodGet, policyPath, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("open editor: %d %s", response.Code, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Version != 1 || len(snapshot.Policy.Groups) != 0 || len(snapshot.Mapping) != 0 || len(snapshot.Roles) != 0 {
		t.Fatalf("discovery inferred owner choices: %+v", snapshot)
	}
	for _, folder := range []string{"INBOX", "[Gmail]/Inbox", "Projects/2026"} {
		if _, ok := snapshot.Folders[folder]; !ok {
			t.Fatalf("missing discovered folder %q: %v", folder, snapshot.Folders)
		}
	}
	var count int
	if err := srv.Store.DB.QueryRow(`SELECT COUNT(*) FROM remote_folders WHERE account_id=?`, created.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("discovery fabricated sync metadata: %d %v", count, err)
	}
	mapping := map[string]string{}
	for remote := range snapshot.Folders {
		mapping[remote] = remote
	}
	mapping["INBOX"], mapping["[Gmail]/Inbox"] = "Inbox", "GmailInbox"
	policy := folderpolicy.Policy{Groups: []folderpolicy.Group{{ID: "chosen", Name: "Personal inbox", Primary: "[Gmail]/Inbox", Folders: []string{"INBOX", "[Gmail]/Inbox"}}}}
	response = voiceRequest(h, "user", http.MethodPut, policyPath, map[string]any{"config_version": snapshot.Version, "policy": policy, "folder_map": mapping, "folder_roles": map[string]string{"inbox": "INBOX"}})
	if response.Code != http.StatusOK {
		t.Fatalf("owner's pre-sync mapping: %d %s", response.Code, response.Body.String())
	}
	response = voiceRequest(h, "user", http.MethodGet, policyPath, nil)
	if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Version != 2 || len(snapshot.Policy.Groups) != 1 || snapshot.Policy.Groups[0].Primary != "[Gmail]/Inbox" || snapshot.Mapping["Projects/2026"] != "Projects/2026" {
		t.Fatalf("reopened owner choices lost: %+v", snapshot)
	}
	// The fixture transport adapters cannot turn a private request endpoint
	// into an allowed one. Policy refuses it before either checker is called.
	if _, err := srv.Store.DB.Exec(`UPDATE accounts SET imap_host='127.0.0.1' WHERE id=?`, created.ID); err != nil {
		t.Fatal(err)
	}
	response = voiceRequest(h, "user", http.MethodPost, "/api/v1/accounts/test", testBody)
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), "policy") {
		t.Fatalf("private endpoint accepted through fixture seam: %d %s", response.Code, response.Body.String())
	}
	if imapChecks.Load() != 1 || smtpChecks.Load() != 1 {
		t.Fatal("blocked endpoint reached provider checks")
	}
}
