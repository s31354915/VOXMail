package calls

import (
	"strings"
	"testing"

	"github.com/voxmail/voxmail/internal/bridge"
	"github.com/voxmail/voxmail/internal/ivr"
	"github.com/voxmail/voxmail/internal/store"
)

func TestHelpTopicsNavigateRepeatAndReturn(t *testing.T) {
	for _, spec := range ivr.States() {
		if !ivr.IsHelpState(spec.State) {
			continue
		}
		for _, key := range []string{"1", "2", "3", "4", "5", "6", "7", "8", "9"} {
			next, valid := ivr.HelpDestination(spec.State, key)
			if !valid {
				continue
			}
			svc := &Service{}
			say := promptRecorder(svc)
			sess := &session{State: string(spec.State), Authenticated: true}
			press := func(key string) {
				t.Helper()
				if err := svc.handleMenu(nil, sess, bridge.Message{Digit: key}); err != nil {
					t.Fatal(err)
				}
			}
			press(key)
			if sess.State != string(next) {
				t.Fatalf("%s + %s reached %s, want %s", spec.State, key, sess.State, next)
			}
			prompt := say()[len(say())-1]
			press("*")
			if sess.State != string(next) || say()[len(say())-1] != prompt {
				t.Fatalf("repeat changed topic %s", next)
			}
			press("#")
			back, _ := ivr.Back(next)
			if sess.State != string(back) {
				t.Fatalf("back from %s reached %s, want %s", next, sess.State, back)
			}
		}
	}
}

func TestConfirmationCanEditOrStartOverWithoutChangingDraft(t *testing.T) {
	for _, key := range []string{"2", "3", "#"} {
		svc := &Service{}
		promptRecorder(svc)
		sess := &session{State: "field_confirm", Authenticated: true, ConfirmState: "compose", ConfirmValue: "sam01+news@example.com", Draft: draft{To: "original@example.com", Body: "keep me"}}
		if err := svc.handleMenu(nil, sess, bridge.Message{Digit: key}); err != nil {
			t.Fatal(err)
		}
		if sess.State != "compose" || sess.Editor == nil {
			t.Fatalf("%s did not reopen editor", key)
		}
		want := "sam01+news@example.com"
		if key == "3" {
			want = ""
		}
		if sess.Editor.Text != want {
			t.Errorf("%s entry=%q, want %q", key, sess.Editor.Text, want)
		}
		if sess.Draft.To != "original@example.com" || sess.Draft.Body != "keep me" {
			t.Fatal("editing changed unconfirmed draft")
		}
	}
}

func TestRepeatIncludesActualChoicesAndReadback(t *testing.T) {
	svc := &Service{}
	tests := []struct {
		sess session
		want string
	}{
		{session{State: "accounts", Accounts: []store.Account{{CanonicalName: "Personal"}}}, "Personal"},
		{session{State: "list", Messages: []store.MailSummary{{Sender: "Sam", Subject: "Meeting"}}}, "Meeting"},
		{session{State: "field_confirm", ConfirmState: "compose", ConfirmValue: "a+1@example.com"}, "plus, one, at"},
	}
	for i := range tests {
		tc := &tests[i]
		if got := svc.promptForState(&tc.sess); !strings.Contains(got, tc.want) {
			t.Errorf("repeat %s: %q missing %q", tc.sess.State, got, tc.want)
		}
	}
}

func TestReviewPreviewStaysAtReviewAndIncludesDraft(t *testing.T) {
	svc := &Service{}
	say := promptRecorder(svc)
	sess := &session{State: "review", Authenticated: true, Draft: draft{To: "sam@example.com", Subject: "Check transcription", Body: "Meet at noon."}}
	if err := svc.handleMenu(nil, sess, bridge.Message{Digit: "6"}); err != nil {
		t.Fatal(err)
	}
	if sess.State != "review" {
		t.Fatalf("preview left review: %s", sess.State)
	}
	for _, want := range []string{"sam@example.com", "Check transcription", "Meet at noon", "1 to send"} {
		if len(say()) != 1 || !strings.Contains(say()[0], want) {
			t.Fatalf("preview=%v missing %q", say(), want)
		}
	}
}
