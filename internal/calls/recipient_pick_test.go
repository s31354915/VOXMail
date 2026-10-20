package calls

import (
	"context"
	"strings"
	"testing"

	"github.com/voxmail/voxmail/internal/store"
)

func pickTestUser(t *testing.T, db *store.Store, id string) {
	t.Helper()
	if err := db.CreateUser(context.Background(), store.User{ID: id, Username: id, Enabled: true}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
}

func TestNewComposeOffersContactChoiceWhenContactsExist(t *testing.T) {
	svc, db := newTestService(t)
	pickTestUser(t, db, "u1")
	if _, err := db.AddContact(context.Background(), store.Contact{UserID: "u1", Name: "Dana", Email: "dana@example.com"}); err != nil {
		t.Fatalf("AddContact: %v", err)
	}
	sess := &session{CallID: "compose-pick", UserID: "u1", State: "draft_menu"}
	svc.startCompose(sess, "")
	if got := string(stateLocked(sess)); got != "recipient_pick" {
		t.Fatalf("state = %q, want recipient_pick", got)
	}
	if sess.Editor != nil {
		t.Fatal("recipient_pick must not open an editor")
	}
}

func TestNewComposeTypesWhenNoContacts(t *testing.T) {
	svc, _ := newTestService(t)
	sess := &session{CallID: "compose-type", UserID: "u2", State: "draft_menu"}
	svc.startCompose(sess, "")
	if got := string(stateLocked(sess)); got != "compose" {
		t.Fatalf("state = %q, want compose", got)
	}
	if sess.Editor == nil {
		t.Fatal("compose must open an editor")
	}
}

func TestRecipientPickOneOpensTyping(t *testing.T) {
	svc := &Service{}
	sess := &session{CallID: "pick-type", State: "recipient_pick"}
	svc.handleRecipientPick(sess, "1")
	if got := string(stateLocked(sess)); got != "compose" {
		t.Fatalf("state = %q, want compose", got)
	}
	if sess.Editor == nil {
		t.Fatal("press 1 must open the address editor")
	}
}

func TestRecipientPickTwoOpensContactList(t *testing.T) {
	svc, db := newTestService(t)
	pickTestUser(t, db, "u1")
	if _, err := db.AddContact(context.Background(), store.Contact{UserID: "u1", Name: "Dana", Email: "dana@example.com"}); err != nil {
		t.Fatalf("AddContact: %v", err)
	}
	sess := &session{CallID: "pick-list", UserID: "u1", State: "recipient_pick"}
	say := promptRecorder(svc)
	svc.handleRecipientPick(sess, "2")
	if got := string(stateLocked(sess)); got != "contact_pick" {
		t.Fatalf("state = %q, want contact_pick", got)
	}
	prompts := say()
	if len(prompts) == 0 || !strings.Contains(prompts[len(prompts)-1], "Dana") {
		t.Fatalf("contact list not announced: %q", prompts)
	}
}

func TestContactPickSelectsMainRecipient(t *testing.T) {
	svc, db := newTestService(t)
	pickTestUser(t, db, "u1")
	if _, err := db.AddContact(context.Background(), store.Contact{UserID: "u1", Name: "Dana", Email: "dana@example.com"}); err != nil {
		t.Fatalf("AddContact: %v", err)
	}
	sess := &session{CallID: "pick-choose", UserID: "u1", State: "contact_pick", Contacts: []store.Contact{{Name: "Dana", Email: "dana@example.com"}}}
	say := promptRecorder(svc)
	svc.handleContactPick(sess, "1")
	if sess.Draft.To != "dana@example.com" {
		t.Fatalf("Draft.To = %q", sess.Draft.To)
	}
	if got := string(stateLocked(sess)); got != "recipient_menu" {
		t.Fatalf("state = %q, want recipient_menu", got)
	}
	prompts := say()
	// The recipient menu announces counts, matching the existing menu's
	// prompt; the picked address landed in Draft.To.
	if len(prompts) == 0 || !strings.Contains(prompts[len(prompts)-1], "1 main") {
		t.Fatalf("recipient menu did not announce the picked recipient: %q", prompts)
	}
}

func TestContactPickInvalidKeyNotices(t *testing.T) {
	svc := &Service{}
	say := promptRecorder(svc)
	sess := &session{CallID: "pick-bad", State: "contact_pick", Contacts: []store.Contact{{Name: "Dana", Email: "dana@example.com"}}}
	svc.handleContactPick(sess, "7")
	prompts := say()
	if len(prompts) == 0 || !strings.Contains(prompts[len(prompts)-1], "no contact for that key") {
		t.Fatalf("missing no-contact notice: %q", prompts)
	}
}
