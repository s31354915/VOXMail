package mailconfig

import (
	"strings"
	"testing"
)

func TestImapUTF7EncodesAmpersandsAndNonASCII(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Sent", "Sent"},
		{"[Gmail]/Spam", "[Gmail]/Spam"},
		{"Con-Ed & National-Grid Bills", "Con-Ed &- National-Grid Bills"},
		{"Amp&Name", "Amp&-Name"},
		{"Grüße", "Gr&APwA3w-e"},
		{"Réponses & transferts", "R&AOk-ponses &- transferts"},
		{"Ünicode Folder", "&ANw-nicode Folder"},
	} {
		if got := imapUTF7(tc.in); got != tc.want {
			t.Fatalf("imapUTF7(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestGenerateEncodesRemoteFolderNamesForIMAP(t *testing.T) {
	a := Account{
		ID: "one", IMAPHost: "imap.example", IMAPUser: "u", MaildirRoot: "/data/mail/one",
		FolderMap: map[string]string{"INBOX": "Inbox", "Con-Ed & National-Grid Bills": "ConEd Bills", "Grüße": "Gruesse"},
	}
	text, err := Generate(a)
	if err != nil {
		t.Fatal(err)
	}
	// The remote (Master) side and the pattern exclusions must carry the
	// modified UTF-7 encoding; the local (Slave) side stays as mapped.
	for _, want := range []string{
		`Master :one-remote:"Con-Ed &- National-Grid Bills"`,
		`Master :one-remote:"Gr&APwA3w-e"`,
		`!"Con-Ed &- National-Grid Bills"`,
		`!"Gr&APwA3w-e"`,
		`Slave :one-local:"ConEd Bills"`,
		`Slave :one-local:"Gruesse"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
	if strings.Contains(text, " & ") {
		t.Fatalf("unencoded remote ampersand leaked into config:\n%s", text)
	}
}
