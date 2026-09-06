package mailconfig

import (
	"strings"
	"testing"
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
