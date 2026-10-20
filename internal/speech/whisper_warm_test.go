package speech

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// whisper-cli rejects any input that is not 16 kHz ("WAV file must be 16
// kHz"), so the silent warm-up fixture must be written at 16 kHz. The fake
// binary copies its input file aside so the test can read the actual header
// Warm fed the recognizer.
func TestWhisperWarmFeedsSixteenKilohertzWAV(t *testing.T) {
	dir := t.TempDir()
	model := filepath.Join(dir, "model")
	if err := os.WriteFile(model, []byte("model"), 0600); err != nil {
		t.Fatal(err)
	}
	fakeWhisper := filepath.Join(dir, "whisper")
	captured := filepath.Join(dir, "captured.wav")
	script := "#!/bin/sh\ncp \"$4\" " + captured + "\n"
	if err := os.WriteFile(fakeWhisper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	warmDir := filepath.Join(dir, "warm")
	if err := (Whisper{Binary: fakeWhisper, Model: model}).Warm(context.Background(), warmDir); err != nil {
		t.Fatalf("warm failed: %v", err)
	}
	data, err := os.ReadFile(captured)
	if err != nil {
		t.Fatalf("warm-up wav was not passed to the recognizer: %v", err)
	}
	if len(data) < 28 {
		t.Fatalf("captured wav too short: %d bytes", len(data))
	}
	sampleRate := binary.LittleEndian.Uint32(data[24:28])
	if sampleRate != 16000 {
		t.Fatalf("warm-up wav sample rate = %d, want 16000", sampleRate)
	}
}
