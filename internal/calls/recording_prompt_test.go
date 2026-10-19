package calls

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/voxmail/voxmail/internal/speech"
)

func TestRecordingStartsAfterSpokenInstructions(t *testing.T) {
	for _, mode := range []string{"subject", "body", "attachment"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			raw, captured := filepath.Join(dir, "call.pcm"), filepath.Join(dir, "captured.pcm")
			if err := os.WriteFile(raw, nil, 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("VOXMAIL_TEST_CAPTURED_PCM", captured)
			converter := writeMediaTestCommand(t, "#!/bin/sh\nprevious=\nfor arg do\nif [ \"$previous\" = -i ]; then cp \"$arg\" \"$VOXMAIL_TEST_CAPTURED_PCM\"; fi\nprevious=\"$arg\"\nlast=\"$arg\"\ndone\nprintf RIFF > \"$last\"\n")
			whisper := writeMediaTestCommand(t, "#!/bin/sh\nprintf 'recognized words'\n")
			svc := &Service{Recorder: &VoiceRecorder{Binary: converter, Whisper: speech.Whisper{Binary: whisper, Model: "fixture"}, Window: 50 * time.Millisecond, Dir: dir}}
			sess := &session{RxPath: raw, Authenticated: true}
			svc.promptFn = func(_ *session, text string) error {
				if strings.HasPrefix(text, "You have up to") {
					// Receive audio accumulates while the guidance is playing.
					return os.WriteFile(raw, bytes.Repeat([]byte{1, 2}, 80), 0600)
				}
				return nil
			}
			switch mode {
			case "subject":
				svc.startSubjectVoice(sess)
			case "body":
				svc.startVoiceRecording(sess)
			case "attachment":
				svc.startAudioAttachment(sess)
			}
			spoken := bytes.Repeat([]byte{3, 4}, 400)
			f, err := os.OpenFile(raw, os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.Write(spoken); err != nil {
				t.Fatal(err)
			}
			f.Close()
			svc.mu.Lock()
			if sess.RecordStop != nil {
				close(sess.RecordStop)
				sess.RecordStop = nil
			}
			svc.mu.Unlock()
			svc.Wait()
			got, err := os.ReadFile(captured)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, spoken) {
				t.Fatalf("captured %d bytes, want only %d bytes spoken after instructions", len(got), len(spoken))
			}
		})
	}
}
