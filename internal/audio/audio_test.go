package audio

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestConvertValidatesRequest(t *testing.T) {
	ctx := context.Background()
	var f FFmpeg
	if err := f.Convert(ctx, "", "out.wav", 16000); err == nil {
		t.Fatal("empty input accepted")
	}
	if err := f.Convert(ctx, "in.wav", "", 16000); err == nil {
		t.Fatal("empty output accepted")
	}
	if err := f.Convert(ctx, "in.wav", "out.wav", 7999); err == nil {
		t.Fatal("too-low sample rate accepted")
	}
}

func TestConvertRunsImpossibleBinary(t *testing.T) {
	err := (FFmpeg{Binary: "voxmail-definitely-not-a-real-binary-xyz"}).Convert(context.Background(), "in", "out", 16000)
	if err == nil {
		t.Fatal("nonexistent binary did not fail")
	}
}

func TestConvertUsesExpectedArguments(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell shim is unix-only")
	}
	dir := t.TempDir()
	shim := filepath.Join(dir, "ffmpeg")
	argsFile := filepath.Join(dir, "args.txt")
	out := filepath.Join(dir, "output.wav")
	content := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\necho fake-wav > " + out + "\n"
	if err := os.WriteFile(shim, []byte(content), 0700); err != nil {
		t.Fatalf("write shim: %v", err)
	}
	err := (FFmpeg{Binary: shim}).Convert(context.Background(), "/tmp/in.mp3", out, 8000)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	data, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(data), "-ar\n8000") || !strings.Contains(string(data), "-ac\n1") || !strings.Contains(string(data), "-f\nwav") {
		t.Fatalf("unexpected ffmpeg arguments: %s", data)
	}
	if b, _ := os.ReadFile(out); string(b) != "fake-wav\n" {
		t.Fatalf("output destination not written: %q", b)
	}
}
