package sip

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestBoundedLogCapsExistingFilesAndKeepsNewestBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "baresip.log")
	for _, name := range []string{path, path + ".1"} {
		if err := os.WriteFile(name, []byte("old log"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	l, err := newBoundedLog(path, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for _, name := range []string{path, path + ".1"} {
		data, err := os.ReadFile(name)
		if err != nil || string(data) != " log" {
			t.Fatalf("existing %s: data=%q err=%v", name, data, err)
		}
		info, err := os.Stat(name)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("existing log is not private: info=%v err=%v", info, err)
		}
	}
	if _, err := l.Write([]byte("new!")); err != nil {
		t.Fatal(err)
	}
	current, _ := os.ReadFile(path)
	previous, _ := os.ReadFile(path + ".1")
	if string(current) != "new!" || string(previous) != " log" {
		t.Fatalf("rotation: current=%q previous=%q", current, previous)
	}
}

func TestBoundedLogConcurrentWritersAndLargeWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "baresip.log")
	l, err := newBoundedLog(path, 97)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var writers sync.WaitGroup
	for i := 0; i < 8; i++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			data := bytes.Repeat([]byte("diagnostic"), 1000)
			n, err := l.Write(data)
			if err != nil || n != len(data) {
				t.Errorf("Write: n=%d err=%v", n, err)
			}
		}()
	}
	writers.Wait()
	for _, name := range []string{path, path + ".1"} {
		info, err := os.Stat(name)
		if err != nil || info.Size() > 97 {
			t.Fatalf("log exceeded cap: info=%v err=%v", info, err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Write([]byte("late write")); err != os.ErrClosed {
		t.Fatalf("closed writer error=%v", err)
	}
}

func TestBoundedLogRotationFailureReturnsPartialWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "baresip.log")
	l, err := newBoundedLog(path, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	// Make the destination unusable after opening the logger; rotation must
	// surface the failure rather than silently grow the current file.
	if err := os.Mkdir(path+".1", 0700); err != nil {
		t.Fatal(err)
	}
	n, err := l.Write([]byte("12345"))
	if n != 4 || err == nil {
		t.Fatalf("rotation failure: n=%d err=%v", n, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "1234" {
		t.Fatalf("failed rotation exceeded bound: data=%q err=%v", data, err)
	}
	if _, err := l.Write([]byte("again")); err != os.ErrClosed {
		t.Fatalf("broken writer error=%v", err)
	}
}

func TestBaresipLogBoundWithoutProcessRestart(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "fake-baresip")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexec \"$VOXMAIL_LOG_TEST_BINARY\" -test.run=^TestSIPLogChild$\n"), 0700); err != nil {
		t.Fatal(err)
	}
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("VOXMAIL_LOG_TEST_BINARY", testBinary)
	t.Setenv("VOXMAIL_LOG_TEST_CHILD", dir)
	path := filepath.Join(dir, "logs", "baresip.log")
	b := &Baresip{Binary: binary, ConfigDir: dir, LogPath: path, LogMaxBytes: 1024, enabled: true}
	b.mu.Lock()
	err = b.startLocked(context.Background())
	first := b.cmd
	b.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Stop)
	for phase := 1; phase <= 2; phase++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprint("phase-", phase)), nil, 0600); err != nil {
			t.Fatal(err)
		}
		want := logTestPayload(phase)
		// Rotation cuts at exact byte boundaries, preserving the previous
		// complete chunk and the final incomplete chunk in chronological order.
		writtenBefore := 0
		if phase == 2 {
			writtenBefore = len(logTestPayload(1))
		}
		all := want
		if phase == 2 {
			all = append(logTestPayload(1), want...)
		}
		retained := 1024 + (writtenBefore+len(want))%1024
		wantTail := all[len(all)-retained:]
		deadline := time.Now().Add(5 * time.Second)
		matched := false
		for time.Now().Before(deadline) {
			previous, _ := os.ReadFile(path + ".1")
			current, _ := os.ReadFile(path)
			if len(previous) > 1024 || len(current) > 1024 {
				t.Fatalf("retained bytes exceeded bound during live run: previous=%d current=%d", len(previous), len(current))
			}
			if bytes.Equal(append(previous, current...), wantTail) {
				matched = true
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !matched {
			t.Fatalf("phase %d: newest output was not retained within two 1024-byte files", phase)
		}
		b.mu.Lock()
		sameChild := b.cmd == first && b.process == ProcessRunning
		b.mu.Unlock()
		if !sameChild || first.Process.Signal(syscall.Signal(0)) != nil {
			t.Fatal("log verification restarted or lost the child process")
		}
	}
}

func logTestPayload(phase int) []byte {
	data := bytes.Repeat([]byte{byte('A' + phase)}, (phase+2)*1024+17)
	return append(data, []byte(fmt.Sprintf("END-PHASE-%d\n", phase))...)
}

// Executed as the real supervised child: emit more than three log limits in
// each phase, then stay alive until the supervisor explicitly stops us.
func TestSIPLogChild(t *testing.T) {
	dir := os.Getenv("VOXMAIL_LOG_TEST_CHILD")
	if dir == "" {
		return
	}
	for phase := 1; phase <= 2; phase++ {
		for {
			if _, err := os.Stat(filepath.Join(dir, fmt.Sprint("phase-", phase))); err == nil {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		output := os.Stdout
		if phase == 2 {
			output = os.Stderr
		}
		if _, err := output.Write(logTestPayload(phase)); err != nil {
			t.Fatal(err)
		}
	}
	for {
		time.Sleep(time.Second)
	}
}
