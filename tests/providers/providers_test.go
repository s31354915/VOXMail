//go:build integration

package providers

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/voxmail/voxmail/internal/operation"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/backend/memory"
	imapclient "github.com/emersion/go-imap/client"
	"github.com/emersion/go-imap/server"
	"github.com/voxmail/voxmail/internal/auth"
	"github.com/voxmail/voxmail/internal/folderpolicy"
	remoteimap "github.com/voxmail/voxmail/internal/imap"
	"github.com/voxmail/voxmail/internal/mailconfig"
	"github.com/voxmail/voxmail/internal/mailer"
	"github.com/voxmail/voxmail/internal/mailindex"
	"github.com/voxmail/voxmail/internal/mailparse"
	"github.com/voxmail/voxmail/internal/mailsync"
	"github.com/voxmail/voxmail/internal/secret"
	"github.com/voxmail/voxmail/internal/store"
	"github.com/voxmail/voxmail/internal/web"
)

func TestRealMbsyncDiscoversFoldersPreservesAttachmentsAndChecksEpoch(t *testing.T) {
	mbsync, err := exec.LookPath("mbsync")
	if err != nil {
		t.Fatal("integration suite requires the real mbsync binary")
	}
	cert, roots, certPEM := certificate(t)
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	backend := &epochBackend{Backend: memory.New()}
	backend.epoch.Store(101)
	imapServer := server.New(backend)
	// The response interceptor hides *tls.Conn from go-imap's type assertion;
	// the underlying listener still requires a verified TLS handshake.
	imapServer.AllowInsecureAuth = true
	disconnects := &disconnectListener{Listener: listener}
	done := make(chan error, 1)
	go func() { done <- imapServer.Serve(disconnects) }()
	t.Cleanup(func() {
		_ = imapServer.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("IMAP fixture did not stop")
		}
	})
	port := listener.Addr().(*net.TCPAddr).Port
	admin, err := imapclient.DialTLS(fmt.Sprintf("localhost:%d", port), &tls.Config{RootCAs: roots, ServerName: "localhost"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.Login("username", "password"); err != nil {
		t.Fatal(err)
	}
	for _, folder := range []string{"Server Sent", "Projects/2026", "Empty", "[Gmail]/Inbox"} {
		if err := admin.Create(folder); err != nil {
			t.Fatal(err)
		}
	}
	payload := []byte("attachment with \x00 binary data and a trailing newline\n")
	raw, err := mailer.BuildMessageWithAttachmentsID("sender@example.test", "Sender", []string{"receiver@example.test"}, nil, nil, "Provider attachment", "Body from the controlled IMAP provider.", []mailer.Attachment{{Filename: "fixture.bin", ContentType: "application/octet-stream", Data: payload}}, "<provider-attachment@example.test>")
	if err != nil {
		t.Fatal(err)
	}
	for _, folder := range []string{"INBOX", "Projects/2026", "Server Sent", "[Gmail]/Inbox"} {
		if err := admin.Append(folder, nil, time.Now(), bytes.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
	}
	if err := admin.Logout(); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	mailRoot := filepath.Join(work, "mail")
	if err := os.MkdirAll(mailRoot, 0700); err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(work, "root.pem")
	if err := os.WriteFile(certPath, certPEM, 0600); err != nil {
		t.Fatal(err)
	}
	account := mailconfig.Account{ID: "fixture", IMAPHost: "localhost", IMAPPort: port, IMAPSecurity: "implicit_tls", IMAPUser: "username", MaildirRoot: mailRoot, FolderMap: map[string]string{"INBOX": "Inbox", "Server Sent": "Sent", "[Gmail]/Inbox": "GmailInbox"}}
	// Production uses authenticated LIST rather than mixing server names with
	// retained local aliases through a wildcard channel.
	discoveryClient, err := remoteimap.Open(context.Background(), remoteimap.Config{Host: "localhost", Port: port, Security: "implicit_tls", Username: "username", Password: "password", TLSConfig: &tls.Config{RootCAs: roots}, AllowPrivate: true})
	if err != nil {
		t.Fatal(err)
	}
	account.RemoteFolders, err = discoveryClient.ListFolders()
	_ = discoveryClient.Close()
	if err != nil {
		t.Fatal(err)
	}
	config, err := mailconfig.Generate(account)
	if err != nil {
		t.Fatal(err)
	}
	// Only the disposable fixture replaces the installed secret helper and adds
	// its private CA. The production config/network policy is not relaxed.
	config = strings.Replace(config, `PassCmd "/usr/local/bin/voxmail-secret fixture"`, `Pass "password"`, 1)
	config = strings.Replace(config, "SSLType IMAPS", "SSLType IMAPS\nCertificateFile "+fmt.Sprintf("%q", certPath)+"\nSystemCertificates no", 1)
	configPath := filepath.Join(work, "mbsyncrc")
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := (mailsync.Runner{Binary: mbsync}).SyncChannels(ctx, configPath, mailconfig.ChannelNames(account)...)
	if err != nil {
		t.Fatalf("real mbsync: %v\n%s", err, result.Output)
	}
	for _, folder := range []string{"Inbox", "Sent", "Projects/2026", "Empty", "GmailInbox"} {
		for _, sub := range []string{"cur", "new", "tmp"} {
			if _, err := os.Stat(filepath.Join(mailRoot, folder, sub)); err != nil {
				t.Fatalf("discovered folder %s/%s missing: %v", folder, sub, err)
			}
		}
	}
	messages, err := mailparse.Scan(mailRoot)
	if err != nil {
		t.Fatal(err)
	}
	matched := 0
	for _, message := range messages {
		if message.MessageID != "<provider-attachment@example.test>" {
			continue
		}
		matched++
		if len(message.Attachments) != 1 || message.Attachments[0].Name != "fixture.bin" || !bytes.Equal(message.Attachments[0].Data, payload) {
			t.Fatalf("attachment changed during real synchronization in %s", message.Folder)
		}
	}
	if matched != 4 {
		t.Fatalf("synced attachment messages=%d, want four across mapped and unmapped folders", matched)
	}
	db, _ := databaseFixture(t, 465)
	if err := (mailindex.Indexer{Store: db}).Index(ctx, "fixture", mailRoot); err != nil {
		t.Fatal(err)
	}
	summaries, err := db.ListMail(ctx, "fixture-user", false)
	if err != nil || len(summaries) != len(messages) {
		t.Fatalf("indexed messages=%d, synced messages=%d, err=%v", len(summaries), len(messages), err)
	}
	for _, summary := range summaries {
		if summary.MessageID == "<provider-attachment@example.test>" && summary.Attachments != 1 {
			t.Fatalf("attachment metadata was not indexed for folder %s", summary.Folder)
		}
	}
	disconnects.armed.Store(true)
	_, syncErr := (mailsync.Runner{Binary: mbsync}).SyncChannels(ctx, configPath, mailconfig.ChannelNames(account)...)
	disconnects.armed.Store(false)
	if syncErr == nil || disconnects.drops.Load() == 0 {
		t.Fatalf("real mbsync did not fail on a controlled mid-FETCH disconnect: err=%v drops=%d", syncErr, disconnects.drops.Load())
	}
	retained, err := mailparse.Scan(mailRoot)
	if err != nil || len(retained) != len(messages) {
		t.Fatalf("failed sync lost previously synchronized mail: retained=%d prior=%d err=%v", len(retained), len(messages), err)
	}
	if connection, err := remoteimap.Open(ctx, remoteimap.Config{Host: "localhost", Port: port, Security: "implicit_tls", Username: "username", Password: "password", AllowPrivate: true}); err == nil {
		_ = connection.Close()
		t.Fatal("IMAP accepted an untrusted provider certificate")
	} else if operation.Classify(err) != operation.TLS {
		t.Fatalf("untrusted IMAP TLS category=%s error=%v", operation.Classify(err), err)
	}
	connection, err := remoteimap.Open(ctx, remoteimap.Config{Host: "localhost", Port: port, Security: "implicit_tls", Username: "username", Password: "password", TLSConfig: &tls.Config{RootCAs: roots}, AllowPrivate: true})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	identities, epoch, err := connection.ListMessageIdentities("INBOX")
	if err != nil || len(identities) == 0 || epoch != 101 {
		t.Fatalf("remote identities=%v epoch=%d err=%v", identities, epoch, err)
	}
	mappingJSON, _ := json.Marshal(account.FolderMap)
	if _, err := db.DB.ExecContext(ctx, `UPDATE accounts SET folder_map=? WHERE id='fixture'`, string(mappingJSON)); err != nil {
		t.Fatal(err)
	}
	for remote, local := range account.FolderMap {
		if err := db.UpsertRemoteFolder(ctx, "fixture", remote, local, ""); err != nil {
			t.Fatal(err)
		}
		remoteMessages, validity, err := connection.ListMessageIdentities(remote)
		if err != nil {
			t.Fatal(err)
		}
		for _, identity := range remoteMessages {
			if err := db.UpdateMailIdentity(ctx, "fixture", local, identity.MessageID, identity.UID, validity, identity.Flags, false); err != nil {
				t.Fatal(err)
			}
		}
	}
	policy := folderpolicy.Policy{Groups: []folderpolicy.Group{{ID: "inbox-pair", Name: "Inbox", Primary: "[Gmail]/Inbox", Folders: []string{"INBOX", "[Gmail]/Inbox"}}}}
	if err := db.SaveFolderPolicy(ctx, "fixture-user", "fixture", 1, policy); err != nil {
		t.Fatal(err)
	}
	grouped, err := db.ListMailForAccount(ctx, "fixture-user", "fixture", "/groups/inbox-pair", false)
	if err != nil || len(grouped) != len(identities)+1 {
		t.Fatalf("provider copies collapsed in group: %+v %v", grouped, err)
	}
	copyCount := 0
	for _, message := range grouped {
		if message.UID == 0 || message.UIDValidity != 101 || message.Folder == "/groups/inbox-pair" {
			t.Fatalf("provider identity lost: %+v", message)
		}
		if message.MessageID == "<provider-attachment@example.test>" {
			copyCount++
		}
	}
	if copyCount != 2 {
		t.Fatalf("distinct provider fixture copies=%d", copyCount)
	}
	primary, local, err := db.GroupDestination(ctx, "fixture-user", "fixture", "/groups/inbox-pair")
	if err != nil || primary != "[Gmail]/Inbox" || local != "GmailInbox" {
		t.Fatalf("provider primary=%q %q %v", primary, local, err)
	}
	var sourceUID uint32
	for _, identity := range identities {
		if identity.MessageID == "<provider-attachment@example.test>" {
			sourceUID = identity.UID
		}
	}
	if sourceUID == 0 {
		t.Fatal("fixture source identity missing")
	}
	if err := connection.MoveByUID("INBOX", primary, sourceUID, epoch); err != nil {
		t.Fatalf("move to owner primary: %v", err)
	}
	state, err := connection.ReconcileMoveByUID("INBOX", sourceUID, epoch)
	// An absent source UID alone cannot prove where a message went; retain the
	// production client's conservative unknown outcome for lost MOVE replies.
	if err != nil || state != remoteimap.MutationStateUnknown {
		t.Fatalf("group move reconciliation=%v %v", state, err)
	}
	moved, _, err := connection.ListMessageIdentities(primary)
	if err != nil || len(moved) != 2 {
		t.Fatalf("primary lost distinct same-Message-ID copies after move: %+v %v", moved, err)
	}
	var cachedPrimary store.MailSummary
	for _, message := range grouped {
		if message.Folder == "GmailInbox" {
			cachedPrimary = message
		}
	}
	if cachedPrimary.UID == 0 {
		t.Fatal("missing original primary-folder identity")
	}
	account.FolderMap[primary] = "RemappedInbox"
	_, version, err := db.FolderPolicySnapshot(ctx, "fixture-user", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveFolderConfiguration(ctx, "fixture-user", "fixture", version, policy, account.FolderMap, map[string]string{"inbox": "INBOX"}); err != nil {
		t.Fatal(err)
	}
	if err := db.BindSyncFolders(ctx, "fixture-user", "fixture", version+1); err != nil {
		t.Fatal(err)
	}
	remappedConfig, err := mailconfig.Generate(account)
	if err != nil {
		t.Fatal(err)
	}
	remappedConfig = strings.Replace(remappedConfig, `PassCmd "/usr/local/bin/voxmail-secret fixture"`, `Pass "password"`, 1)
	remappedConfig = strings.Replace(remappedConfig, "SSLType IMAPS", "SSLType IMAPS\nCertificateFile "+fmt.Sprintf("%q", certPath)+"\nSystemCertificates no", 1)
	if err := os.WriteFile(configPath, []byte(remappedConfig), 0600); err != nil {
		t.Fatal(err)
	}
	remappedResult, err := (mailsync.Runner{Binary: mbsync}).SyncChannels(ctx, configPath, mailconfig.ChannelNames(account)...)
	if err != nil {
		t.Fatalf("real mbsync remap: %v\n%s", err, remappedResult.Output)
	}
	if err := (mailindex.Indexer{Store: db}).Index(ctx, "fixture", mailRoot); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertRemoteFolder(ctx, "fixture", primary, "RemappedInbox", ""); err != nil {
		t.Fatal(err)
	}
	remappedGroup, err := db.ListMailForAccount(ctx, "fixture-user", "fixture", "/groups/inbox-pair", false)
	if err != nil {
		t.Fatal(err)
	}
	retainedOld, newCopies := false, 0
	for _, message := range remappedGroup {
		retainedOld = retainedOld || message.ID == cachedPrimary.ID
		if message.Folder == "RemappedInbox" && message.MessageID == "<provider-attachment@example.test>" {
			newCopies++
		}
	}
	if !retainedOld || newCopies != 2 {
		t.Fatalf("real remap lost old/new indexed members: old=%v new=%d", retainedOld, newCopies)
	}
	historicalRemote, err := db.RemoteFolderForLocal(ctx, "fixture-user", "fixture", cachedPrimary.Folder)
	if err != nil || historicalRemote != primary {
		t.Fatalf("old cached source changed during remap: %q %v", historicalRemote, err)
	}
	if err := connection.SetSeenByUID(historicalRemote, cachedPrimary.UID, cachedPrimary.UIDValidity, true); err != nil {
		t.Fatalf("seen mutation through retained source: %v", err)
	}
	hasSeen := func(flags []string) bool {
		for _, flag := range flags {
			if strings.EqualFold(flag, `\Seen`) {
				return true
			}
		}
		return false
	}
	afterSeen, _, err := connection.ListMessageIdentities(primary)
	if err != nil {
		t.Fatal(err)
	}
	for _, identity := range afterSeen {
		if identity.UID == cachedPrimary.UID {
			if !hasSeen(identity.Flags) {
				t.Fatal("retained UID was not marked seen")
			}
			continue
		}
		for _, before := range moved {
			if before.UID == identity.UID && hasSeen(before.Flags) != hasSeen(identity.Flags) {
				t.Fatal("remapped mutation changed a distinct same-Message-ID copy")
			}
		}
	}
	backend.epoch.Store(102)
	if err := connection.SetSeenByUID(historicalRemote, cachedPrimary.UID, cachedPrimary.UIDValidity, false); !errors.Is(err, remoteimap.ErrUIDValidityChanged) {
		t.Fatalf("retained binding bypassed UIDVALIDITY change: %v", err)
	}
	if err := connection.SetSeenByUID("INBOX", identities[0].UID, epoch, false); !errors.Is(err, remoteimap.ErrUIDValidityChanged) {
		t.Fatalf("stale identity survived a server UIDVALIDITY change: %v", err)
	}
}

func TestVerifiedSMTPPreservesAttachmentsAndAcceptanceOutcomes(t *testing.T) {
	for _, mode := range []string{"accept", "quit-drop", "data-drop"} {
		t.Run(mode, func(t *testing.T) {
			port, roots, received := smtpFixture(t, mode, "sender", "secret")
			payload := []byte("SMTP attachment\x00\n")
			raw, err := mailer.BuildMessageWithAttachmentsID("sender@example.test", "Sender", []string{"receiver@example.test"}, nil, []string{"hidden@example.test"}, "SMTP integration", "Verified TLS attachment test.", []mailer.Attachment{{Filename: "file.bin", ContentType: "application/octet-stream", Data: payload}}, "<smtp-integration@example.test>")
			if err != nil {
				t.Fatal(err)
			}
			result := mailer.SendWithOutcome(mailer.Config{Host: "localhost", Port: port, Security: "implicit_tls", Username: "sender", Password: "secret", From: "sender@example.test", AllowPrivate: true, TLSConfig: &tls.Config{RootCAs: roots}}, []string{"receiver@example.test", "hidden@example.test"}, raw)
			want := mailer.SendAccepted
			if mode == "data-drop" {
				want = mailer.SendUncertain
			}
			if result.Status != want || (mode == "quit-drop" && result.CleanupError == nil) || (mode == "data-drop" && (result.Err == nil || operation.Classify(result.Err) != operation.Uncertain || !errors.Is(result.Err, operation.ErrOutcomeUncertain))) {
				t.Fatalf("SMTP %s result=%+v, want %s", mode, result, want)
			}
			select {
			case delivery := <-received:
				message, err := mailparse.Parse(bytes.NewReader(delivery.Raw))
				if err != nil || message.MessageID != "<smtp-integration@example.test>" || len(message.Attachments) != 1 || !bytes.Equal(message.Attachments[0].Data, payload) {
					t.Fatalf("SMTP received a changed message: %+v err=%v", message, err)
				}
				if len(delivery.Recipients) != 2 || bytes.Contains(delivery.Raw, []byte("Bcc:")) {
					t.Fatal("SMTP envelope or Bcc privacy failed")
				}
			default:
				t.Fatal("provider did not receive DATA")
			}
		})
	}
}

func TestSMTPRejectsUntrustedCertificateBeforeSending(t *testing.T) {
	port, _, received := smtpFixture(t, "accept", "sender", "secret")
	result := mailer.SendWithOutcome(mailer.Config{Host: "localhost", Port: port, Security: "implicit_tls", Username: "sender", Password: "secret", From: "sender@example.test", AllowPrivate: true, TLSConfig: &tls.Config{InsecureSkipVerify: true}}, []string{"receiver@example.test"}, []byte("Subject: must not arrive\r\n\r\nbody"))
	if result.Status != mailer.SendRejected || result.Err == nil || !strings.Contains(result.Err.Error(), "certificate") || operation.Classify(result.Err) != operation.TLS {
		t.Fatalf("untrusted TLS was accepted: %+v", result)
	}
	select {
	case <-received:
		t.Fatal("SMTP DATA was sent after certificate rejection")
	default:
	}
}

func TestRecoveryEmailDeliveredByVerifiedSMTPRedeemsExactlyOnce(t *testing.T) {
	for _, mode := range []string{"accept", "quit-drop", "data-drop"} {
		t.Run(mode, func(t *testing.T) {
			port, roots, received := smtpFixture(t, mode, "recovery", "smtp-secret")
			db, secrets := databaseFixture(t, port)
			svc := &web.Server{Store: db, Secrets: secrets, SendRecovery: func(ctx context.Context, cfg mailer.Config, recipients []string, raw []byte) mailer.SendResult {
				cfg.AllowPrivate = true
				cfg.TLSConfig = &tls.Config{RootCAs: roots}
				return mailer.SendWithOutcomeContext(ctx, cfg, recipients, raw)
			}}
			handler := svc.Handler()
			request := httptest.NewRequest(http.MethodPost, "/api/v1/recovery/request", bytes.NewBufferString(`{"email":"recovery@example.test","purpose":"reset_password"}`))
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusAccepted {
				t.Fatalf("recovery request status=%d: %s", recorder.Code, recorder.Body.String())
			}
			var delivery smtpDelivery
			select {
			case delivery = <-received:
			default:
				t.Fatal("recovery email did not reach the authenticated provider")
			}
			message, err := mailparse.Parse(bytes.NewReader(delivery.Raw))
			if err != nil {
				t.Fatal(err)
			}
			match := regexp.MustCompile(`recovery code is ([0-9]{6})\.`).FindStringSubmatch(message.OriginalText)
			if len(match) != 2 || len(delivery.Recipients) != 1 || delivery.Recipients[0] != "recovery@example.test" {
				t.Fatal("recovery recipient or code was not delivered correctly")
			}
			payload, _ := json.Marshal(map[string]string{"email": "recovery@example.test", "purpose": "reset_password", "code": match[1], "new_password": "NewRecoveryPassword-123"})
			want := http.StatusOK
			if mode == "data-drop" {
				want = http.StatusUnauthorized // uncertain delivery invalidates the code
			}
			for attempt := 0; attempt < 2; attempt++ {
				request := httptest.NewRequest(http.MethodPost, "/api/v1/recovery/confirm", bytes.NewReader(payload))
				request.Header.Set("Content-Type", "application/json")
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, request)
				if recorder.Code != want {
					t.Fatalf("recovery %s confirmation %d status=%d, want %d: %s", mode, attempt, recorder.Code, want, recorder.Body.String())
				}
				want = http.StatusUnauthorized
			}
			user, err := db.UserByID(context.Background(), "fixture-user")
			if err != nil {
				t.Fatal(err)
			}
			if auth.Check(user.PasswordHash, "NewRecoveryPassword-123") != (mode != "data-drop") {
				t.Fatal("recovery changed the password contrary to SMTP acceptance")
			}
		})
	}
}

func databaseFixture(t *testing.T, smtpPort int) (*store.Store, *secret.Box) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "provider.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	secrets, err := secret.New("provider-integration-disposable-key-0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	password, err := auth.Hash("OldRecoveryPassword-123")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateUser(context.Background(), store.User{ID: "fixture-user", Username: "fixture-user", PasswordHash: password, PINHash: password, Role: "user", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveAccount(context.Background(), secrets, store.Account{ID: "fixture", UserID: "fixture-user", CanonicalName: "Provider fixture", Email: "recovery@example.test", IMAPHost: "imap.example.test", IMAPPort: 993, IMAPSecurity: "implicit_tls", IMAPUser: "username", IMAPPassword: "password", SMTPHost: "localhost", SMTPPort: smtpPort, SMTPSecurity: "implicit_tls", SMTPUser: "recovery", SMTPPassword: "smtp-secret", FolderMap: `{}`, AlertFolders: `[]`, SyncIntervalMinutes: 5, ReconciliationIntervalMinutes: 1440}); err != nil {
		t.Fatal(err)
	}
	return db, secrets
}
