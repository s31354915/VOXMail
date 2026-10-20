package speech

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The transcription flags must pin English, strip timestamps, disable
// temperature fallback, tighten the no-speech threshold, and suppress
// non-speech tokens, so telephone hiss neither drifts language nor feeds
// hallucinated filler to Piper. The wav stays the last argument because
// the recording stubs read it to know which file to fabricate.
func TestWhisperTranscribeFlags(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "whisper-stub")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\"\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	wav := filepath.Join(dir, "call.wav")
	w := Whisper{Binary: stub, Model: filepath.Join(dir, "model.bin")}
	text, err := w.Transcribe(context.Background(), wav)
	if err != nil {
		t.Fatalf("Transcribe error=%v", err)
	}
	args := strings.Split(text, "\n")
	want := []string{
		"-m", w.Model,
		"-l", "en",
		"--no-timestamps",
		"--no-fallback",
		"--no-speech-thold", whisperNoSpeechThreshold,
		"--suppress-nst",
		"--prompt", whisperPrompt,
		"--no-prints",
		"-f", wav,
	}
	if len(args) != len(want) {
		t.Fatalf("args=%q, want %q", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args[%d]=%q, want %q (full %q)", i, args[i], want[i], args)
		}
	}
}
