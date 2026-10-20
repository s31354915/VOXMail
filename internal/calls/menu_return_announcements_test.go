package calls

import (
	"strings"
	"testing"

	"github.com/voxmail/voxmail/internal/store"
)

func TestAccountMenuNoticesRepeatAccountList(t *testing.T) {
	svc := &Service{}
	say := promptRecorder(svc)
	sess := &session{CallID: "acct-menu", UserID: "u", State: "accounts", Accounts: []store.Account{{ID: "a1", CanonicalName: "work@example.com"}}}
	svc.handleAccountMenu(sess, "0")
	prompts := say()
	if len(prompts) == 0 {
		t.Fatal("expected an invalid-key notice")
	}
	last := prompts[len(prompts)-1]
	if !strings.Contains(last, "not available") {
		t.Fatalf("missing invalid-key notice: %q", last)
	}
	if !strings.Contains(last, "work@example.com") {
		t.Fatalf("notice did not repeat the accounts menu: %q", last)
	}
}

func TestContactMenuEndOfListRepeatsContacts(t *testing.T) {
	svc := &Service{}
	say := promptRecorder(svc)
	sess := &session{CallID: "contact-menu", UserID: "u", State: "contacts", Contacts: []store.Contact{{ID: 1, Name: "Dana", Email: "dana@example.com"}}}
	svc.handleContactMenu(sess, "9")
	prompts := say()
	if len(prompts) == 0 {
		t.Fatal("expected an end-of-list notice")
	}
	last := prompts[len(prompts)-1]
	if !strings.Contains(last, "no more contacts") {
		t.Fatalf("missing end-of-list notice: %q", last)
	}
	if !strings.Contains(last, "Dana") {
		t.Fatalf("notice did not repeat the contacts menu: %q", last)
	}
}

func TestDraftListNoticesRepeatDraftPage(t *testing.T) {
	svc := &Service{}
	say := promptRecorder(svc)
	sess := &session{CallID: "draft-list", UserID: "u", State: "draft_list", DraftPage: 0, Drafts: []store.DraftRecord{{ID: "d1", Subject: "lab report"}}}
	svc.handleDraftList(sess, "0")
	prompts := say()
	if len(prompts) < 2 {
		t.Fatalf("expected notice plus draft-page repeat, got %v", prompts)
	}
	if !strings.Contains(prompts[0], "not available") {
		t.Fatalf("missing invalid-key notice: %q", prompts[0])
	}
	if !strings.Contains(prompts[1], "lab report") {
		t.Fatalf("notice did not repeat the draft list: %q", prompts[1])
	}
}

func TestDraftListEmptyListStaysQuietAfterNotice(t *testing.T) {
	svc := &Service{}
	say := promptRecorder(svc)
	sess := &session{CallID: "draft-list-empty", UserID: "u", State: "draft_list", DraftPage: 0}
	svc.handleDraftList(sess, "0")
	prompts := say()
	if len(prompts) != 1 {
		t.Fatalf("empty list must not re-announce the same notice, got %v", prompts)
	}
}
