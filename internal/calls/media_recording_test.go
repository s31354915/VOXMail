package calls

import (
	"context"
	"errors"
	"github.com/voxmail/voxmail/internal/bridge"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/voxmail/voxmail/internal/maintenance"
	"github.com/voxmail/voxmail/internal/speech"
)

func TestRecorderOwnsTempsThroughoutBlockedConversion(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "recordings")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	raw := filepath.Join(root, "capture.pcm")
	if err := os.WriteFile(raw, make([]byte, 128), 0600); err != nil {
		t.Fatal(err)
	}
	entered := filepath.Join(root, "conversion-entered")
	release := filepath.Join(root, "conversion-release")
	t.Setenv("VOXMAIL_TEST_CONVERSION_ENTERED", entered)
	t.Setenv("VOXMAIL_TEST_CONVERSION_RELEASE", release)
	converter := writeMediaTestCommand(t, "#!/bin/sh\nlast=\nfor arg do last=\"$arg\"; done\n: > \"$VOXMAIL_TEST_CONVERSION_ENTERED\"\nwhile [ ! -f \"$VOXMAIL_TEST_CONVERSION_RELEASE\" ]; do sleep 0.01; done\nprintf RIFF > \"$last\"\n")
	whisper := writeMediaTestCommand(t, "#!/bin/sh\nprintf 'recognized text\\n'\n")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stop := make(chan struct{})
	close(stop)
	done := make(chan error, 1)
	go func() {
		text, err := (&VoiceRecorder{Binary: converter, Dir: dir, Whisper: speech.Whisper{Binary: whisper, Model: "fixture"}}).RecordAndTranscribeUntil(ctx, nil, raw, 0, stop)
		if err == nil && text != "recognized text" {
			err = errors.New("unexpected transcription")
		}
		done <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(entered); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("conversion did not start")
		}
		time.Sleep(time.Millisecond)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("recording fixture: entries=%d err=%v", len(entries), err)
	}
	if report, err := maintenance.CleanupEphemeral(root, time.Now().Add(48*time.Hour), time.Hour); err != nil || report.RemovedFiles != 0 {
		t.Fatalf("maintenance removed owned conversion input/output: %+v %v", report, err)
	}
	for _, entry := range entries {
		if _, err := os.Stat(filepath.Join(dir, entry.Name())); err != nil {
			t.Fatal("conversion lost an active file", err)
		}
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("conversion did not finish")
	}
	entries, err = os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("recording cleanup failed: entries=%d err=%v", len(entries), err)
	}
}

func TestVoiceRecorderCancellationStopsBeforeFilesystemWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := (&VoiceRecorder{}).RecordAndTranscribe(ctx, nil, filepath.Join(t.TempDir(), "missing.pcm"), 0)
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
	}).RecordAndTranscribeUntil(context.Background(), nil, rawPath, 0, stop)
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

func TestAudioStopFinishesWhileCancellationAborts(t *testing.T) {
	dir := t.TempDir()
	raw := filepath.Join(dir, "capture.pcm")
	if err := os.WriteFile(raw, make([]byte, 128), 0600); err != nil {
		t.Fatal(err)
	}
	converter := writeMediaTestCommand(t, "#!/bin/sh\nlast=\nfor arg do last=\"$arg\"; done\nprintf 'RIFF' > \"$last\"\n")
	stop := make(chan struct{})
	close(stop)
	recorder := &VoiceRecorder{Binary: converter, Dir: dir}
	data, err := recorder.RecordAudioUntil(context.Background(), raw, 0, stop)
	if err != nil || string(data) != "RIFF" {
		t.Fatalf("finished capture=%q err=%v", data, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = recorder.RecordAudioUntil(ctx, filepath.Join(dir, "missing.pcm"), 0, stop)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error=%v", err)
	}
}

func TestAudioConversionRemainsOwnedByCallContext(t *testing.T) {
	dir := t.TempDir()
	raw := filepath.Join(dir, "capture.pcm")
	if err := os.WriteFile(raw, make([]byte, 128), 0600); err != nil {
		t.Fatal(err)
	}
	converter := writeMediaTestCommand(t, "#!/bin/sh\nsleep 30\n")
	stop := make(chan struct{})
	close(stop)
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := (&VoiceRecorder{Binary: converter, Dir: dir}).RecordAudioUntil(ctx, raw, 0, stop)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("conversion error=%v", err)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("conversion outlived its call context")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".attachment-") {
			t.Fatalf("temporary media remains: %s", entry.Name())
		}
	}
}

func TestAudioFinishKeyStopsCaptureWithoutCancelingConversion(t *testing.T) {
	svc, _ := newTestService(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := make(chan struct{})
	sess := &session{CallID: "audio-finish", State: "audio_recording", Authenticated: true, RecordCancel: cancel, RecordStop: stop}
	svc.sessions[sess.CallID] = sess
	if err := svc.handleMenu(nil, sess, bridge.Message{Type: "dtmf", CallID: sess.CallID, Digit: "0", Phase: "start"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stop:
	default:
		t.Fatal("finish key did not stop capture")
	}
	if ctx.Err() != nil {
		t.Fatal("finish key canceled conversion ownership")
	}
}

func TestAudioConversionRejectsOversizedOutputAndCleansFile(t *testing.T) {
	dir := t.TempDir()
	raw := filepath.Join(dir, "capture.pcm")
	if err := os.WriteFile(raw, make([]byte, 128), 0600); err != nil {
		t.Fatal(err)
	}
	converter := writeMediaTestCommand(t, "#!/bin/sh\nlast=\nfor arg do last=\"$arg\"; done\nprintf '%08388609d' 0 > \"$last\"\n")
	stop := make(chan struct{})
	close(stop)
	data, err := (&VoiceRecorder{Binary: converter, Dir: dir}).RecordAudioUntil(context.Background(), raw, 0, stop)
	if err == nil || !strings.Contains(err.Error(), "exceeds size limit") || data != nil {
		t.Fatalf("data length=%d err=%v", len(data), err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".attachment-") {
			t.Fatalf("oversized temporary file remains: %s", entry.Name())
		}
	}
}

// Regression test: the recorder must wait on the call's activated speech
// runtime, not the recorder's own never-activated Runtime. With the runtime
// pool, the base runtime is never Activated, so its sttReady never exists and
// WaitWhisper failed with "speech runtime is not active" on every recording.
func TestVoiceRecorderTranscribesThroughActivatedCallRuntime(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "recordings")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	raw := filepath.Join(root, "capture.pcm")
	if err := os.WriteFile(raw, make([]byte, 128), 0600); err != nil {
		t.Fatal(err)
	}
	converter := writeMediaTestCommand(t, "#!/bin/sh\nlast=\nfor arg do last=\"$arg\"; done\nprintf RIFF > \"$last\"\n")
	whisper := writeMediaTestCommand(t, "#!/bin/sh\nprintf 'recognized text\\n'\n")
	model := filepath.Join(root, "model.bin")
	if err := os.WriteFile(model, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	callRuntime := speech.NewRuntime(speech.Piper{Model: model}, speech.Whisper{Binary: whisper, Model: model}, dir)
	lease := callRuntime.Activate()
	defer lease.Release()
	// The recorder's own runtime is deliberately never activated: its
	// sttReady stays nil for the whole test.
	recorder := &VoiceRecorder{
		Runtime: speech.NewRuntime(speech.Piper{Model: model}, speech.Whisper{Binary: whisper, Model: model}, dir),
		Binary:  converter,
		Dir:     dir,
		Whisper: speech.Whisper{Binary: whisper, Model: model},
	}
	stop := make(chan struct{})
	close(stop)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	text, err := recorder.RecordAndTranscribeUntil(ctx, callRuntime, raw, 0, stop)
	if err != nil {
		t.Fatalf("RecordAndTranscribeUntil error=%v", err)
	}
	if text != "recognized text" {
		t.Fatalf("text=%q, want recognized text", text)
	}
}
