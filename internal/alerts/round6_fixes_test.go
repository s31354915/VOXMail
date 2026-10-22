package alerts

import (
	"testing"

	"github.com/voxmail/voxmail/internal/store"
)

func TestFriendlyFolderName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"INBOX.Genealogy/Correspondence", "Genealogy Correspondence"},
		{"INBOX.Sent", "Sent"},
		{"INBOX", "INBOX"},
		{"", "your inbox"},
		{"  Inbox/  ", "Inbox"},
	}
	for _, c := range cases {
		if got := friendlyFolderName(c.in); got != c.want {
			t.Errorf("friendlyFolderName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestAlertTextUsesFriendlyFolderNames(t *testing.T) {
	list := []store.AlertCandidate{{Folder: "INBOX.Genealogy/Correspondence"}}
	if got := alertText(list, alertPromptContext{}); got != "You have new email in Genealogy Correspondence." {
		t.Fatalf("alertText=%q", got)
	}
}
