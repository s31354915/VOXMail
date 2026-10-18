package calls

import (
	"strings"
	"testing"
	"time"

	"github.com/voxmail/voxmail/internal/mailer"
)

// promptRecorder captures prompts the way the production prompt path would
// speak them, without needing media playback.
func promptRecorder(svc *Service) func() []string {
	var messages []string
	svc.promptFn = func(_ *session, text string) error {
		messages = append(messages, text)
		return nil
	}
	return func() []string { return messages }
}

func TestRound6AccountMenuInvalidKeyNotices(t *testing.T) {
	svc := &Service{}
	say := promptRecorder(svc)
	sess := &session{}
	svc.handleAccountMenu(sess, "5")
	if got := len(say()); got != 1 || !strings.Contains(say()[0], "no account for that key") {
		t.Fatalf("prompts=%v, want an out-of-range notice", say())
	}
}

func TestRound6AccountMenuMoreWithoutPagesNotices(t *testing.T) {
	svc := &Service{}
	say := promptRecorder(svc)
	sess := &session{}
	svc.handleAccountMenu(sess, "9")
	if got := len(say()); got != 1 || !strings.Contains(say()[0], "no more accounts") {
		t.Fatalf("prompts=%v, want an end-of-list notice", say())
	}
}

func TestRound6ContactMenuInvalidKeysNotice(t *testing.T) {
	svc := &Service{}
	say := promptRecorder(svc)
	sess := &session{}
	svc.handleContactMenu(sess, "0")
	if got := len(say()); got != 1 || !strings.Contains(say()[0], "not available") {
		t.Fatalf("prompts=%v, want an invalid-key notice", say())
	}
	sess.Contacts = nil
	svc.handleContactMenu(sess, "9")
	if got := len(say()); got != 2 || !strings.Contains(say()[1], "no more contacts") {
		t.Fatalf("prompts=%v, want an end-of-list notice", say())
	}
}

func TestRound6DraftListEndOfListNotices(t *testing.T) {
	svc := &Service{}
	say := promptRecorder(svc)
	sess := &session{}
	svc.handleDraftList(sess, "9")
	if got := len(say()); got != 1 || !strings.Contains(say()[0], "no more saved drafts") {
		t.Fatalf("prompts=%v, want an end-of-list notice", say())
	}
	svc.handleDraftList(sess, "5")
	if got := len(say()); got != 2 || !strings.Contains(say()[1], "no saved draft for that key") {
		t.Fatalf("prompts=%v, want an out-of-range notice", say())
	}
}

func TestRound6AttachmentEditRemovalRequiresSecondPress(t *testing.T) {
	svc := &Service{}
	say := promptRecorder(svc)
	sess := &session{Draft: draft{Attachments: []mailer.Attachment{{Filename: "a.wav"}, {Filename: "b.wav"}}}}
	svc.handleAttachmentEdit(sess, "2")
	if len(sess.Draft.Attachments) != 2 {
		t.Fatalf("first press removed an attachment; want a confirmation first")
	}
	if got := len(say()); got != 1 || !strings.Contains(say()[0], "Press 2 again") {
		t.Fatalf("prompts=%v, want a removal confirmation", say())
	}
	svc.handleAttachmentEdit(sess, "2")
	if len(sess.Draft.Attachments) != 1 {
		t.Fatalf("second press did not remove the attachment")
	}
	if got := len(say()); got != 2 || !strings.Contains(say()[1], "was removed") {
		t.Fatalf("prompts=%v, want a removal notice", say())
	}
}

func TestRound6FieldReentryKeepsTypedText(t *testing.T) {
	svc := &Service{}
	say := promptRecorder(svc)
	sess := &session{ConfirmValue: "sam@example.com", ConfirmState: "compose"}
	svc.handleFieldConfirmation(sess, "2")
	if sess.Editor == nil || sess.Editor.Text != "sam@example.com" {
		t.Fatalf("editor text=%v, want the previously typed value", sess.Editor)
	}
	if got := len(say()); got != 1 || !strings.Contains(say()[0], "Editing your previous entry") {
		t.Fatalf("prompts=%v, want an edit notice", say())
	}
}

func TestRound6ReviewCancelRequiresSecondPress(t *testing.T) {
	svc := &Service{}
	say := promptRecorder(svc)
	sess := &session{}
	svc.mu.Lock()
	transitionLocked(sess, "review")
	svc.mu.Unlock()
	svc.handleReviewCancel(sess)
	if got := len(say()); got != 1 || !strings.Contains(say()[0], "Press 3 again") {
		t.Fatalf("prompts=%v, want a cancel confirmation", say())
	}
	svc.handleReviewCancel(sess)
	svc.mu.Lock()
	state := string(stateLocked(sess))
	svc.mu.Unlock()
	if state != "main" {
		t.Fatalf("state=%q, want main after the second press", state)
	}
}

func TestRound6PINFailureAnnouncesRemainingAttempts(t *testing.T) {
	if notice := pinFailureNotice(2); !strings.Contains(notice, "2 attempts remain") {
		t.Fatalf("notice=%q, want the remaining count", notice)
	}
	if notice := pinFailureNotice(1); !strings.Contains(notice, "One attempt remains") {
		t.Fatalf("notice=%q, want the last-attempt wording", notice)
	}
}

func TestRound6RecordingAndGraceConstantsStayInSync(t *testing.T) {
	if dtmfReleaseGrace != 250*time.Millisecond {
		t.Fatalf("dtmfReleaseGrace=%v, want 250ms so fast repeat presses survive", dtmfReleaseGrace)
	}
	if defaultSpeechWindow != 30*time.Second {
		t.Fatalf("defaultSpeechWindow=%v, want 30s to match the announced cap", defaultSpeechWindow)
	}
}
