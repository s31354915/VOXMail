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
