package calls

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeConversionFFmpeg writes a fake ffmpeg binary whose behavior depends on
// the requested filter chain: loudness measurement fails, conversions with
// loudness processing fail with recorded stderr, and the plain telephone
// filter conversion either succeeds with the payload or fails too.
func fakeConversionFFmpeg(t *testing.T, payload []byte, plainFails bool) (binary, logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "argv.log")
	data := filepath.Join(dir, "out.pcm")
	if payload != nil {
		if err := os.WriteFile(data, payload, 0600); err != nil {
			t.Fatal(err)
		}
	}
	plainAction := fmt.Sprintf("cat '%s'", data)
	if plainFails {
		plainAction = "echo 'plain conversion failed too' >&2; exit 1"
	}
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + logPath + "\n" +
		"case \"$*\" in\n" +
		"  *print_format=json*) echo 'loudness measurement unavailable' >&2; exit 1;;\n" +
		"  *loudnorm*|*alimiter*|*volume=*) echo 'filtered conversion failed' >&2; exit 1;;\n" +
		"  *) " + plainAction + ";;\n" +
		"esac\n"
	binary = filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return binary, logPath
}

// TestPromptConversionFallsBackToPlainFilter covers the loudness-free retry:
// when every conversion that carries loudness processing fails but the same
// input converts with the plain telephone filter, the prompt must still play
// instead of silently failing.
func TestPromptConversionFallsBackToPlainFilter(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "tx.pcm")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	binary, logPath := fakeConversionFFmpeg(t, bytes.Repeat([]byte{0x01, 0x02}, 400), false)
	wavPath := filepath.Join(t.TempDir(), "prompt.wav")
	if err := os.WriteFile(wavPath, []byte("RIFF synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	player := &PromptPlayer{Binary: binary}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	// A consumer stands in for the call's baresip reader: playback completes
	// only once the converted PCM has actually been consumed. The reader
	// opens non-blocking (a blocking FIFO open would deadlock against the
	// writer that playback opens later) and drains in a loop, tolerating
	// EAGAIN while the FIFO is empty.
	done := make(chan error, 1)
	consumer, err := os.OpenFile(fifo, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer consumer.Close()
	go func() {
		buf := make([]byte, 8192)
		for {
			_, _ = consumer.Read(buf)
			select {
			case <-done:
				return
			default:
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	go func() { done <- player.playWAVTail(ctx, fifo, wavPath, 0) }()
	err = <-done
	if err != nil {
		logs, _ := os.ReadFile(logPath)
		t.Fatalf("plain-filter fallback did not deliver the prompt: %v\nffmpeg invocations:\n%s", err, logs)
	}
	invocations, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(invocations), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 ffmpeg invocations (measure, filtered conversion, plain retry), got %d:\n%s", len(lines), invocations)
	}
	if !strings.Contains(lines[1], "loudnorm") {
		t.Fatalf("the first conversion should have used loudness processing:\n%s", lines[1])
	}
	if strings.Contains(lines[2], "loudnorm") || strings.Contains(lines[2], "alimiter") || strings.Contains(lines[2], "volume=") {
		t.Fatalf("the retry should bypass loudness processing:\n%s", lines[2])
	}
}

// TestPromptConversionFailureCarriesDiagnostics covers the report ask: a
// failed conversion must say which input, which filter chain, how much audio
// reached the call, and what ffmpeg wrote to stderr, for both attempts.
func TestPromptConversionFailureCarriesDiagnostics(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "tx.pcm")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	binary, _ := fakeConversionFFmpeg(t, nil, true)
	wavPath := filepath.Join(t.TempDir(), "prompt.wav")
	if err := os.WriteFile(wavPath, []byte("RIFF synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	player := &PromptPlayer{Binary: binary}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	reader, err := os.OpenFile(fifo, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	err = player.playWAVTail(ctx, fifo, wavPath, 0)
	if err == nil {
		t.Fatal("expected the conversion to fail")
	}
	t.Logf("final error: %v", err)
	text := err.Error()
	for _, want := range []string{
		"filtered conversion failed",
		"plain conversion failed too",
		wavPath,
		"aformat=channel_layouts=mono,aresample=8000",
		"played 0 bytes",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("error is missing %q:\n%s", want, text)
		}
	}
}
