package calls

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Regression for the round-7 WaitDelay truncation: a converter that exits
// while the paced writer is still draining must not lose the prompt's tail.
// Before the fix, exec's output pipe discarded everything still buffered
// once the child exited and WaitDelay (500 ms) lapsed, which callers heard
// as menus cut off mid-sentence.
func TestPlayInputDeliversEntirePromptAfterConverterExits(t *testing.T) {
	payload := bytes.Repeat([]byte{0x55, 0xAA}, 24000) // 48000 bytes: 6 s of audio
	player, fifo := fakePCMPlayer(t, payload)
	reader, err := os.OpenFile(fifo, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	readDone := make(chan int, 1)
	readErr := make(chan error, 1)
	go func() {
		data, err := io.ReadAll(reader)
		readDone <- len(data)
		readErr <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err = player.PlayMedia(ctx, fifo, "ignored")
	if err != nil {
		t.Fatalf("PlayMedia: %v", err)
	}
	select {
	case got := <-readDone:
		if got != len(payload) {
			t.Fatalf("FIFO received %d of %d bytes: the prompt tail was truncated", got, len(payload))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reader never reached EOF")
	}
	if err := <-readErr; err != nil {
		t.Fatalf("FIFO read: %v", err)
	}
}

// A converter that fails without producing audio must still surface its
// exit status instead of a silent prompt.
func TestPlayInputReportsConverterFailure(t *testing.T) {
	player, fifo := fakePCMPlayer(t, []byte{1, 2, 3, 4})
	broken := filepath.Join(filepath.Dir(fifo), "broken-ffmpeg")
	if err := os.WriteFile(broken, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	player.Binary = broken
	reader, err := os.OpenFile(fifo, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = player.PlayMedia(ctx, fifo, "ignored")
	if err == nil {
		t.Fatal("PlayMedia: want conversion failure, got nil")
	}
	if !strings.Contains(err.Error(), "prompt conversion") {
		t.Fatalf("error %v does not name the conversion failure", err)
	}
}

func TestRederiveMaildirPathFindsFlagVariant(t *testing.T) {
	cur := filepath.Join(t.TempDir(), "cur")
	if err := os.MkdirAll(cur, 0700); err != nil {
		t.Fatal(err)
	}
	unread := filepath.Join(cur, "12345.example:2,")
	if err := os.WriteFile(unread, []byte("body"), 0600); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(cur, "12345.example:2,S")
	got, ok := rederiveMaildirPath(stale)
	if !ok || got != unread {
		t.Fatalf("rederiveMaildirPath(%q) = %q, %v; want %q", stale, got, ok, unread)
	}
}

func TestRederiveMaildirPathScansFlagSiblings(t *testing.T) {
	cur := filepath.Join(t.TempDir(), "cur")
	if err := os.MkdirAll(cur, 0700); err != nil {
		t.Fatal(err)
	}
	onDisk := filepath.Join(cur, "12345.example:2,SR")
	if err := os.WriteFile(onDisk, []byte("body"), 0600); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(cur, "12345.example:2,S")
	got, ok := rederiveMaildirPath(stale)
	if !ok || got != onDisk {
		t.Fatalf("rederiveMaildirPath(%q) = %q, %v; want %q", stale, got, ok, onDisk)
	}
	if _, ok := rederiveMaildirPath(filepath.Join(cur, "missing.example:2,S")); ok {
		t.Fatal("rederiveMaildirPath found a message that does not exist")
	}
}

type eagainWriter struct {
	calls   int
	written int
}

func (w *eagainWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls == 1 {
		return 0, &fs.PathError{Op: "write", Path: "fifo", Err: syscall.EAGAIN}
	}
	w.written += len(p)
	return len(p), nil
}

func TestPacedWriterRetriesEAGAIN(t *testing.T) {
	sink := &eagainWriter{}
	writer := &pacedPCMWriter{ctx: context.Background(), w: sink}
	if _, err := writer.Write(bytes.Repeat([]byte{0}, 160)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if sink.calls != 2 {
		t.Fatalf("writes=%d, want one EAGAIN retry", sink.calls)
	}
	if sink.written != 160 {
		t.Fatalf("delivered %d bytes, want 160", sink.written)
	}
}

func TestCopyConvertedPCMDrainsToEOF(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "source.pcm")
	payload := bytes.Repeat([]byte{7}, 70000) // larger than one read buffer
	if err := os.WriteFile(path, payload, 0600); err != nil {
		t.Fatal(err)
	}
	src, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	var sink bytes.Buffer
	if err := copyConvertedPCM(context.Background(), src, &sink); err != nil {
		t.Fatalf("copyConvertedPCM: %v", err)
	}
	if !bytes.Equal(sink.Bytes(), payload) {
		t.Fatalf("drained %d bytes, want %d", sink.Len(), len(payload))
	}
}

func TestCopyConvertedPCMPropagatesWriteErrors(t *testing.T) {
	dir := t.TempDir()
	src, err := os.Create(filepath.Join(dir, "source.pcm"))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if _, err := src.WriteString("audio"); err != nil {
		t.Fatal(err)
	}
	src.Seek(0, io.SeekStart)
	err = copyConvertedPCM(context.Background(), src, errWriter{})
	if err == nil || !errors.Is(err, errBoom) {
		t.Fatalf("error=%v, want errBoom", err)
	}
}

type errWriter struct{}

var errBoom = errors.New("boom")

func (errWriter) Write(p []byte) (int, error) { return 0, errBoom }
