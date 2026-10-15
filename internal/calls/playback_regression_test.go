package calls

import (
	"bytes"
	"context"
	"errors"
	"github.com/voxmail/voxmail/internal/bridge"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func fakePCMPlayer(t *testing.T, payload []byte) (*PromptPlayer, string) {
	t.Helper()
	dir := t.TempDir()
	fifo := filepath.Join(dir, "tx.pcm")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, "audio.pcm")
	if err := os.WriteFile(data, payload, 0600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\ncat '"+data+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return &PromptPlayer{Binary: binary}, fifo
}

func TestPlaybackWaitsForFIFOConsumption(t *testing.T) {
	payload := bytes.Repeat([]byte{1, 2}, 1600) // 200 ms, fits entirely in the FIFO
	player, fifo := fakePCMPlayer(t, payload)
	reader, err := os.OpenFile(fifo, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- player.PlayMedia(ctx, fifo, "ignored") }()
	// Even after the full payload was written, an unread FIFO is not playback.
	select {
	case err := <-done:
		t.Fatalf("playback completed before consumption: %v", err)
	case <-time.After(350 * time.Millisecond):
	}
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("received %d bytes, want %d", len(got), len(payload))
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("playback did not finish after consumption")
	}
}

func TestPlaybackCancellationWithNonConsumingReader(t *testing.T) {
	player, fifo := fakePCMPlayer(t, bytes.Repeat([]byte{1, 2}, 16000))
	reader, err := os.OpenFile(fifo, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	// Fill the FIFO so the first converted frame blocks in Go's output copy.
	filler, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer filler.Close()
	fd := int(filler.Fd())
	for {
		_, err = syscall.Write(fd, make([]byte, 4096))
		if err == syscall.EAGAIN {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- player.PlayMedia(ctx, fifo, "ignored") }()
	time.Sleep(80 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("DTMF cancellation remained blocked on the full FIFO")
	}
}

func TestPCMPacingLimitsBufferedSpeech(t *testing.T) {
	var output bytes.Buffer
	writer := &pacedPCMWriter{ctx: context.Background(), w: &output}
	start := time.Now()
	payload := make([]byte, pcmBytesPerSecond/5)
	if _, err := writer.Write(payload); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 130*time.Millisecond {
		t.Fatal("200ms of PCM was queued as a burst")
	}
	if output.Len() != len(payload) {
		t.Fatal("PCM was lost")
	}
}

func TestAlertCompletionWaitsForActualPlayback(t *testing.T) {
	payload := bytes.Repeat([]byte{1, 2}, 1600)
	player, fifo := fakePCMPlayer(t, payload)
	wav := filepath.Join(t.TempDir(), "alert.wav")
	if err := os.WriteFile(wav, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	player.Static = map[string]string{"alert": wav}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan bool, 1)
	sess := &session{CallID: "alert", State: "outgoing", TxPath: fifo, AlertText: "alert", AlertDone: done}
	svc := &Service{Media: player, ctx: ctx, sessions: map[string]*session{"alert": sess}}
	reader, err := os.OpenFile(fifo, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	finished := make(chan struct{})
	go func() { svc.playOutgoingAlert(sess); close(finished) }()
	select {
	case <-done:
		t.Fatal("alert marked completed before any audio was consumed")
	case <-time.After(350 * time.Millisecond):
	}
	if got, err := io.ReadAll(reader); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("alert PCM bytes=%d err=%v", len(got), err)
	}
	select {
	case success := <-done:
		if !success {
			t.Fatal("consumed alert failed")
		}
	case <-time.After(time.Second):
		t.Fatal("alert did not complete")
	}
	<-finished
}

func TestUnavailableAlertAudioIsNotSuccessful(t *testing.T) {
	done := make(chan bool, 1)
	svc := &Service{}
	svc.playOutgoingAlert(&session{CallID: "alert", State: "outgoing", AlertText: "alert", AlertDone: done})
	if <-done {
		t.Fatal("silent alert reported success")
	}
}

func TestLongPlaybackPausesInactivityAndUsesMediaBudget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc := &Service{ctx: ctx, InactivityTimeout: 30 * time.Millisecond}
	sess := &session{CallID: "read", State: "read"}
	goodbye := make(chan struct{}, 1)
	svc.promptFn = func(*session, string) error { goodbye <- struct{}{}; return nil }
	svc.mu.Lock()
	svc.armInactivityTimeoutLocked(sess)
	svc.mu.Unlock()
	err := svc.playPrompt(sess, func(playCtx context.Context) error {
		deadline, _ := playCtx.Deadline()
		if time.Until(deadline) < 10*time.Minute {
			t.Error("speech still uses a short call setup timeout")
		}
		select {
		case <-goodbye:
			t.Error("inactivity interrupted active playback")
		case <-time.After(100 * time.Millisecond):
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-goodbye:
	case <-time.After(time.Second):
		t.Fatal("inactivity did not resume after playback")
	}
	cancel()
	svc.StopTasks()
	svc.Wait()
}

func TestDuplicateEstablishedDoesNotRestartAlertOrGreet(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	sess := &session{CallID: "alert", State: "outgoing", AlertText: "alert", TxPath: "fixture"}
	svc := &Service{ctx: ctx, Media: &PromptPlayer{}, sessions: map[string]*session{"alert": sess}}
	svc.promptFn = func(*session, string) error { started <- struct{}{}; <-release; return nil }
	svc.onEstablished(bridge.Message{Type: "call_established", CallID: "alert"})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("alert did not start")
	}
	svc.onEstablished(bridge.Message{Type: "call_established", CallID: "alert"})
	select {
	case <-started:
		t.Error("duplicate establishment started another prompt")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	cancel()
	svc.StopTasks()
	svc.Wait()
}

func TestPlaybackFailureIsLoggedBeforeCleanupCancelsContext(t *testing.T) {
	var logs bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc := &Service{ctx: ctx, Log: slog.New(slog.NewTextHandler(&logs, nil))}
	expected := errors.New("decoder fixture failed")
	err := svc.playPrompt(&session{State: "announce"}, func(context.Context) error { return expected })
	if !errors.Is(err, expected) || !strings.Contains(logs.String(), "decoder fixture failed") {
		t.Fatalf("error=%v log=%s", err, logs.String())
	}
}
