package calls

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/voxmail/voxmail/internal/speech"
)

func TestVoiceRecorderCancellationStopsBeforeFilesystemWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := (&VoiceRecorder{}).RecordAndTranscribe(ctx, filepath.Join(t.TempDir(), "missing.pcm"), 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RecordAndTranscribe error=%v, want context.Canceled", err)
	}
}

func TestVoiceRecorderRecordAudioUsesBoundedSegmentAndCleansTemps(t *testing.T) {
	dir := t.TempDir()
	rawPath := filepath.Join(dir, "capture.pcm")
	input := make([]byte, 8000*2)
	if err := os.WriteFile(rawPath, input, 0600); err != nil {
		t.Fatal(err)
	}
	converter := writeMediaTestCommand(t, "#!/bin/sh\nlast=\nfor arg do last=\"$arg\"; done\nprintf 'RIFF' > \"$last\"\n")

	data, err := (&VoiceRecorder{Binary: converter, Dir: dir, Window: time.Second}).RecordAudio(context.Background(), rawPath, 1)
	if err != nil {
		t.Fatalf("RecordAudio error=%v", err)
	}
	if string(data) != "RIFF" {
		t.Fatalf("converted data=%q, want fake WAV marker", data)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == "capture.pcm" || entry.Name() == filepath.Base(converter) {
			continue
		}
		if entry.Name()[0] == '.' {
			t.Fatalf("temporary media artifact remained: %s", entry.Name())
		}
	}
}

func TestVoiceRecorderTranscribesStoppedCaptureAndRemovesWAV(t *testing.T) {
	dir := t.TempDir()
	rawPath := filepath.Join(dir, "capture.pcm")
	if err := os.WriteFile(rawPath, make([]byte, 128), 0600); err != nil {
		t.Fatal(err)
	}
	converter := writeMediaTestCommand(t, "#!/bin/sh\nlast=\nfor arg do last=\"$arg\"; done\nprintf 'RIFF' > \"$last\"\n")
	whisper := writeMediaTestCommand(t, "#!/bin/sh\nprintf 'recognized text\\n'\n")
	stop := make(chan struct{})
	close(stop)

	text, err := (&VoiceRecorder{
		Binary:  converter,
		Dir:     dir,
		Window:  time.Hour,
		Whisper: speech.Whisper{Binary: whisper, Model: "test-model"},
	}).RecordAndTranscribeUntil(context.Background(), rawPath, 0, stop)
	if err != nil {
		t.Fatalf("RecordAndTranscribeUntil error=%v", err)
	}
	if text != "recognized text" {
		t.Fatalf("transcription=%q, want recognized text", text)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == "capture.pcm" {
			continue
		}
		if entry.Name()[0] == '.' {
			t.Fatalf("temporary media artifact remained: %s", entry.Name())
		}
	}
}

func writeMediaTestCommand(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "media-command")
	if err := os.WriteFile(path, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}
