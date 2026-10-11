package speech

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/voxmail/voxmail/internal/operation"
	"github.com/voxmail/voxmail/internal/process"
)

func TestWhisperRejectsTruncatedTranscriptionAndRemovesRecording(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "whisper")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf '%02097152d' 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	wav := filepath.Join(dir, "recording.wav")
	if err := os.WriteFile(wav, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	text, err := (Whisper{Binary: binary, Model: "fixture"}).TranscribeAndRemove(context.Background(), wav)
	if err == nil || !strings.Contains(err.Error(), "exceeds output limit") || text != "" || operation.Classify(err) != operation.ResourceLimit {
		t.Fatalf("text length=%d err=%v", len(text), err)
	}
	if _, err := os.Stat(wav); !os.IsNotExist(err) {
		t.Fatalf("raw recording remained: %v", err)
	}
}

func TestWhisperCancellationBoundsNoisyChildAndPreservesCause(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "whisper")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nwhile :; do printf 'diagnostic diagnostic\\n' >&2; done\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := (Whisper{Binary: binary, Model: "fixture"}).Transcribe(ctx, "fixture.wav")
	if !errors.Is(err, context.DeadlineExceeded) || operation.Classify(err) != operation.Timeout {
		t.Fatalf("error=%v", err)
	}
	if len(err.Error()) > process.MaxOutput+512 {
		t.Fatalf("unbounded error length=%d", len(err.Error()))
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("noisy recognition child did not stop")
	}
}
