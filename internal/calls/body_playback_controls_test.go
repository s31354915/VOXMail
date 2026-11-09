package calls

import (
	"strings"
	"testing"
)

func newPlaybackTestService(t *testing.T) (*Service, *session, *[]string) {
	t.Helper()
	svc, _ := newTestService(t)
	sess := &session{CallID: "playback", State: "read", Authenticated: true}
	svc.sessions[sess.CallID] = sess
	prompts := &[]string{}
	svc.promptFn = func(_ *session, text string) error {
		*prompts = append(*prompts, text)
		return nil
	}
	return svc, sess, prompts
}

func lastPrompt(prompts *[]string) string {
	if len(*prompts) == 0 {
		return ""
	}
	return (*prompts)[len(*prompts)-1]
}

func anyPrompt(prompts *[]string, sub string) bool {
	for _, text := range *prompts {
		if strings.Contains(text, sub) {
			return true
		}
	}
	return false
}

// Interrupting a playing body must turn keys 4, 6 and 7 into pause, skip
// and speech rate, and pausing must resume from the interrupted chunk.
func TestHandleBodyPlaybackKeyPauseAndResume(t *testing.T) {
	svc, sess, prompts := newPlaybackTestService(t)
	svc.mu.Lock()
	sess.BodyChunks = []string{"one", "two", "three"}
	sess.BodyIndex = 1
	sess.BodyInterrupted = true
	svc.mu.Unlock()

	if !svc.handleBodyPlaybackKey(sess, "4") {
		t.Fatal("key 4 during body playback was not consumed")
	}
	if !strings.Contains(lastPrompt(prompts), "Paused. Press 4 to keep going") {
		t.Fatalf("pause prompt %q", lastPrompt(prompts))
	}
	svc.mu.Lock()
	if !sess.BodyPaused {
		t.Fatal("session not marked paused")
	}
	svc.mu.Unlock()

	// Resuming replays from the interrupted chunk and ends on the menu.
	if !svc.handleBodyPlaybackKey(sess, "4") {
		t.Fatal("key 4 while paused was not consumed")
	}
	if !strings.Contains(lastPrompt(prompts), "Press 0 for the next message") {
		t.Fatalf("resume tail %q", lastPrompt(prompts))
	}
	svc.mu.Lock()
	if sess.BodyIndex != 3 {
		t.Fatalf("BodyIndex=%d, want 3", sess.BodyIndex)
	}
	if sess.BodyPaused || sess.BodyInterrupted {
		t.Fatal("playback state not cleared after resume")
	}
	svc.mu.Unlock()
}

func TestHandleBodyPlaybackKeySkipAndSpeed(t *testing.T) {
	svc, sess, prompts := newPlaybackTestService(t)
	svc.mu.Lock()
	sess.BodyChunks = []string{"one", "two", "three"}
	sess.BodyIndex = 0
	sess.BodyInterrupted = true
	svc.mu.Unlock()

	if !svc.handleBodyPlaybackKey(sess, "6") {
		t.Fatal("key 6 during body playback was not consumed")
	}
	if !anyPrompt(prompts, "Skipping ahead") {
		t.Fatalf("skip prompt missing: %v", *prompts)
	}
	svc.mu.Lock()
	if sess.BodyIndex != 3 {
		t.Fatalf("skip left BodyIndex=%d, want 3", sess.BodyIndex)
	}
	svc.mu.Unlock()

	// Speed cycles normal, faster, slower, normal.
	cycles := []struct {
		want   int
		prompt string
	}{
		{5, "Speaking faster"},
		{1, "Speaking slower"},
		{0, "Normal speech rate"},
	}
	for _, want := range cycles {
		svc.mu.Lock()
		sess.BodyInterrupted = true
		svc.mu.Unlock()
		if !svc.handleBodyPlaybackKey(sess, "7") {
			t.Fatal("key 7 was not consumed")
		}
		if !anyPrompt(prompts, want.prompt) {
			t.Fatalf("speed prompt missing for %d: %v", want.want, *prompts)
		}
		svc.mu.Lock()
		if sess.BodySpeed != want.want {
			t.Fatalf("BodySpeed=%d, want %d", sess.BodySpeed, want.want)
		}
		svc.mu.Unlock()
	}
}

// Without a live or paused body, keys 4, 6 and 7 keep their read-menu
// meanings, and any other key on a paused body ends the playback context.
func TestHandleBodyPlaybackKeyIdleKeysUnaffected(t *testing.T) {
	svc, sess, _ := newPlaybackTestService(t)
	svc.mu.Lock()
	sess.BodyChunks = []string{"one", "two"}
	svc.mu.Unlock()
	if svc.handleBodyPlaybackKey(sess, "4") {
		t.Fatal("key 4 at an idle read menu was consumed as a playback control")
	}
	svc.mu.Lock()
	sess.BodyPaused = true
	svc.mu.Unlock()
	if svc.handleBodyPlaybackKey(sess, "3") {
		t.Fatal("key 3 on a paused body was consumed as a playback control")
	}
	svc.mu.Lock()
	if sess.BodyChunks != nil || sess.BodyPaused {
		t.Fatal("non-control key did not end the playback context")
	}
	svc.mu.Unlock()
}

// The read menu recites fully the first time a message is read on the call,
// then falls back to a short tier with the rest behind star.
func TestReadMenuPromptTiers(t *testing.T) {
	svc, sess, _ := newPlaybackTestService(t)
	first := svc.readMenuPrompt(sess)
	if !strings.Contains(first, "8 for attachments") {
		t.Fatalf("first read menu was not the full recitation: %q", first)
	}
	second := svc.readMenuPrompt(sess)
	if !strings.Contains(second, "Press star to hear the rest of the controls") {
		t.Fatalf("second read menu was not the short tier: %q", second)
	}
	if strings.Contains(second, "8 for attachments") {
		t.Fatalf("short tier should not recite every control: %q", second)
	}
}
