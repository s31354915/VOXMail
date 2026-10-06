package mailconfig

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/voxmail/voxmail/internal/secret"
	"github.com/voxmail/voxmail/internal/store"
)

func TestGenerateDoesNotCreateRemoteFolders(t *testing.T) {
	text, err := Generate(Account{ID: "gmail/one", IMAPHost: "imap.example", IMAPUser: "u", MaildirRoot: "/data/mail/gmail-one"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "Create Slave") || strings.Contains(text, "Create Both") || strings.Contains(text, "Create Master") {
		t.Fatalf("unsafe folder creation policy:\n%s", text)
	}
	if !strings.Contains(text, "Expunge None") || strings.Contains(text, "Pass \"p\"") {
		t.Fatalf("unsafe deletion or credential policy:\n%s", text)
	}
	if !strings.Contains(text, "Patterns *") {
		t.Fatal("all remote folders are not selected")
	}
}

func TestGenerateAppliesFolderAliases(t *testing.T) {
	text, err := Generate(Account{ID: "one", IMAPHost: "imap.example", IMAPUser: "u", MaildirRoot: "/data/mail/one", FolderMap: map[string]string{"INBOX": "Inbox", "Archive": "Old Mail"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Inbox \"/data/mail/one/Inbox\"", "Patterns * !\"Archive\"", "Master :one-remote:\"Archive\"", "Slave :one-local:\"Old Mail\""} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing folder mapping %q in:\n%s", want, text)
		}
	}
}

func TestGenerateRejectsEscapingFolderAlias(t *testing.T) {
	if _, err := Generate(Account{ID: "one", IMAPHost: "imap.example", IMAPUser: "u", MaildirRoot: "/data/mail/one", FolderMap: map[string]string{"Archive": "../../outside"}}); err == nil {
		t.Fatal("path escaping folder alias was accepted")
	}
}

func TestChannelNamesMatchGeneratedMappedChannels(t *testing.T) {
	account := Account{ID: "gmail/one", IMAPHost: "imap.example", IMAPUser: "u", MaildirRoot: "/data/mail/one", FolderMap: map[string]string{
		"INBOX":             "Inbox",
		"Archive":           "Old Mail",
		"[Gmail]/Sent Mail": "Sent",
	}}
	text, err := Generate(account)
	if err != nil {
		t.Fatal(err)
	}
	channels := ChannelNames(account)
	if len(channels) != 3 || channels[0] != "gmail_one" || channels[1] != "gmail_one-folder-1" || channels[2] != "gmail_one-folder-2" {
		t.Fatalf("channels=%v", channels)
	}
	for _, channel := range channels {
		if !strings.Contains(text, "Channel "+channel) {
			t.Fatalf("generated config does not contain %q:\n%s", channel, text)
		}
	}
}

func TestGenerateUsesStartTLSWhenConfigured(t *testing.T) {
	text, err := Generate(Account{ID: "one", IMAPHost: "imap.example", IMAPPort: 143, IMAPSecurity: "STARTTLS", IMAPUser: "u", MaildirRoot: "/data/mail/one"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "SSLType STARTTLS") || !strings.Contains(text, "Port 143") {
		t.Fatalf("STARTTLS configuration missing or incorrect:\n%s", text)
	}
}

func TestGeneratePinsTunnelWithoutReplacingTLSHost(t *testing.T) {
	text, err := Generate(Account{ID: "one", IMAPHost: "imap.example", IMAPAddress: "198.51.100.8:993", IMAPPort: 993, IMAPUser: "u", MaildirRoot: "/data/mail/one"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, `Host "imap.example"`) {
		t.Fatalf("TLS hostname was not retained:\n%s", text)
	}
	if !strings.Contains(text, `Tunnel "/usr/local/bin/voxmail-connect 198.51.100.8:993"`) {
		t.Fatalf("pinned tunnel was not generated:\n%s", text)
	}
	if !strings.Contains(text, "SSLType IMAPS") {
		t.Fatalf("TLS mode was not retained:\n%s", text)
	}
}

func TestGenerateRejectsDisallowedTunnelAddress(t *testing.T) {
	if _, err := Generate(Account{ID: "one", IMAPHost: "imap.example", IMAPAddress: "127.0.0.1:993", IMAPPort: 993, IMAPUser: "u", MaildirRoot: "/data/mail/one"}); err == nil {
		t.Fatal("disallowed tunnel address was accepted")
	}
}

func TestGenerateMapsPersistedImplicitTLSMode(t *testing.T) {
	text, err := Generate(Account{ID: "one", IMAPHost: "imap.example", IMAPPort: 993, IMAPSecurity: "implicit_tls", IMAPUser: "u", MaildirRoot: "/data/mail/one"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "SSLType IMAPS") || strings.Contains(text, "SSLType IMPLICIT_TLS") {
		t.Fatalf("persisted implicit_tls was not translated to IMAPS:\n%s", text)
	}
}

func TestGenerateDefaultsPort143ToStartTLS(t *testing.T) {
	text, err := Generate(Account{ID: "one", IMAPHost: "imap.example", IMAPPort: 143, IMAPUser: "u", MaildirRoot: "/data/mail/one"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "SSLType STARTTLS") {
		t.Fatalf("port 143 default did not select STARTTLS:\n%s", text)
	}
}

func TestSavedAccountUsesCanonicalSecurityInGeneratedConfig(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "account.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.CreateUser(ctx, store.User{ID: "u1", Username: "alice", PasswordHash: "hash", PINHash: "pin", Role: "user", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	box, err := secret.New("test-key-with-more-than-32-characters-123456")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveAccount(ctx, box, store.Account{
		ID: "a1", UserID: "u1", CanonicalName: "Work", Email: "alice@example.com", SenderName: "Alice",
		IMAPHost: "imap.example.com", IMAPPort: 993, IMAPUser: "alice", IMAPPassword: "imap-password",
		SMTPHost: "smtp.example.com", SMTPPort: 465, SMTPUser: "alice", SMTPPassword: "smtp-password",
		FolderMap: `{}`, IMAPSecurity: "IMAPS", SMTPSecurity: "implicit_tls",
	}); err != nil {
		t.Fatal(err)
	}
	accounts, err := db.ListAccounts(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || accounts[0].IMAPSecurity != "implicit_tls" {
		t.Fatalf("stored account security=%q, want implicit_tls; accounts=%d", accounts[0].IMAPSecurity, len(accounts))
	}
	text, err := Generate(Account{
		ID: "a1", IMAPHost: accounts[0].IMAPHost, IMAPPort: accounts[0].IMAPPort,
		IMAPSecurity: accounts[0].IMAPSecurity, IMAPUser: accounts[0].IMAPUser,
		MaildirRoot: "/data/mail/a1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "SSLType IMAPS") || strings.Contains(text, "SSLType IMPLICIT_TLS") {
		t.Fatalf("saved account generated invalid TLS configuration:\n%s", text)
	}
}
