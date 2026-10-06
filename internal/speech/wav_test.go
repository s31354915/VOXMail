package speech

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTestWAV(path string) error {
	data := []byte{0, 0}
	var file bytes.Buffer
	file.WriteString("RIFF")
	_ = binary.Write(&file, binary.LittleEndian, uint32(38))
	file.WriteString("WAVEfmt ")
	_ = binary.Write(&file, binary.LittleEndian, uint32(16))
	_ = binary.Write(&file, binary.LittleEndian, uint16(1))
	_ = binary.Write(&file, binary.LittleEndian, uint16(1))
	_ = binary.Write(&file, binary.LittleEndian, uint32(8000))
	_ = binary.Write(&file, binary.LittleEndian, uint32(16000))
	_ = binary.Write(&file, binary.LittleEndian, uint16(2))
	_ = binary.Write(&file, binary.LittleEndian, uint16(16))
	file.WriteString("data")
	_ = binary.Write(&file, binary.LittleEndian, uint32(len(data)))
	file.Write(data)
	return os.WriteFile(path, file.Bytes(), 0600)
}

func TestValidateWAVRejectsIncompleteOrInconsistentFiles(t *testing.T) {
	root := t.TempDir()
	valid := filepath.Join(root, "valid.wav")
	if err := writeTestWAV(valid); err != nil {
		t.Fatal(err)
	}
	if err := validateWAV(valid); err != nil {
		t.Fatalf("valid WAV rejected: %v", err)
	}
	data, err := os.ReadFile(valid)
	if err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string][]byte{
		"truncated-data": data[:len(data)-1],
		"extra-byte":     append(append([]byte(nil), data...), 0),
		"bad-riff-size": func() []byte {
			copy := append([]byte(nil), data...)
			binary.LittleEndian.PutUint32(copy[4:8], 0)
			return copy
		}(),
	} {
		path := filepath.Join(root, name+".wav")
		if err := os.WriteFile(path, contents, 0600); err != nil {
			t.Fatal(err)
		}
		if err := validateWAV(path); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func writeFakePiper(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-piper.sh")
	script := "#!/bin/sh\nout=\nnext=0\nfor arg in \"$@\"; do if [ \"$next\" = 1 ]; then out=$arg; next=0; elif [ \"$arg\" = \"--output_file\" ]; then next=1; elif [ \"$arg\" = \"--json-input\" ]; then exit 91; fi; done\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

const partialWAVBody = "printf 'RIFF\\050\\000\\000\\000WAVEfmt \\020\\000\\000\\000\\001\\000\\001\\000\\100\\037\\000\\000\\200\\076\\000\\000\\002\\000\\020\\000data\\004\\000\\000\\000\\000\\000' > \"$out\""

func TestPiperSynthesisWaitsForProcessAndPublishesAtomically(t *testing.T) {
	piper := writeFakePiper(t, partialWAVBody+"\nsleep 0.25\nprintf '\\000\\000' >> \"$out\"")
	model := filepath.Join(t.TempDir(), "model.onnx")
	if err := os.WriteFile(model, []byte("model"), 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "published.wav")
	done := make(chan error, 1)
	go func() { done <- (Piper{Binary: piper, Model: model}).Synthesize(context.Background(), "hello", output) }()
	time.Sleep(80 * time.Millisecond)
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("output was published before process completion: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := validateWAV(output); err != nil {
		t.Fatalf("published WAV invalid: %v", err)
	}
}

func TestPiperSynthesisNormalizesTextBeforeLaunchingPiper(t *testing.T) {
	root := t.TempDir()
	seen := filepath.Join(root, "input.txt")
	piper := writeFakePiper(t, "cat > "+shellQuote(seen)+"\n"+partialWAVBody+"\nprintf '\\000\\000' >> \"$out\"")
	model := filepath.Join(root, "model.onnx")
	if err := os.WriteFile(model, []byte("model"), 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "published.wav")
	if err := (Piper{Binary: piper, Model: model}).Synthesize(context.Background(), "Welcome to VOXMail. Enter your PIN.", output); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(seen)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "Welcome to Vox Mail. Enter your pin.\n"; got != want {
		t.Fatalf("Piper received %q, want %q", got, want)
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func TestPiperSynthesisRejectsMidWriteExitAndPreservesPreviousOutput(t *testing.T) {
	piper := writeFakePiper(t, partialWAVBody+"\nexit 0")
	model := filepath.Join(t.TempDir(), "model.onnx")
	if err := os.WriteFile(model, []byte("model"), 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "published.wav")
	if err := writeTestWAV(output); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	err = (Piper{Binary: piper, Model: model}).Synthesize(context.Background(), "hello", output)
	if err == nil || !strings.Contains(err.Error(), "invalid WAV") {
		t.Fatalf("mid-write exit error = %v, want invalid WAV", err)
	}
	after, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("failed synthesis replaced the previous published WAV")
	}
}

func TestPiperSynthesisCancellationDoesNotPublishOrLeaveStage(t *testing.T) {
	piper := writeFakePiper(t, partialWAVBody+"\nsleep 5")
	model := filepath.Join(t.TempDir(), "model.onnx")
	if err := os.WriteFile(model, []byte("model"), 0600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	output := filepath.Join(root, "published.wav")
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	err := (Piper{Binary: piper, Model: model}).Synthesize(ctx, "hello", output)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation error = %v, want deadline exceeded", err)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("canceled output exists: %v", err)
	}
	entries, err := filepath.Glob(filepath.Join(root, ".piper-output-*.wav"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("canceled staging files remain: %v", entries)
	}
}

func TestPiperWorkerUsesPinnedPlainTextCLIAndCloseCancels(t *testing.T) {
	piper := writeFakePiper(t, partialWAVBody+"\nsleep 5")
	model := filepath.Join(t.TempDir(), "model.onnx")
	if err := os.WriteFile(model, []byte("model"), 0600); err != nil {
		t.Fatal(err)
	}
	worker, err := startPiperWorker(context.Background(), Piper{Binary: piper, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "published.wav")
	done := make(chan error, 1)
	go func() { done <- worker.synthesize(context.Background(), "hello", output) }()
	time.Sleep(80 * time.Millisecond)
	worker.close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("closed worker reported successful synthesis")
		}
	case <-time.After(time.Second):
		t.Fatal("worker close did not cancel the active Piper process")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("closed worker published output: %v", err)
	}
}

func TestCacheRejectsInvalidSynthesizedAudio(t *testing.T) {
	root := t.TempDir()
	c := Cache{Root: root, Synth: invalidSynth{}}
	if _, err := c.Build(context.Background(), "voice", 3, map[string]string{"welcome": "hello"}); err == nil {
		t.Fatal("cache accepted invalid synthesized audio")
	}
	if _, err := os.Stat(filepath.Join(root, "active")); !os.IsNotExist(err) {
		t.Fatalf("invalid build activated cache: %v", err)
	}
}

type invalidSynth struct{}

func (invalidSynth) Synthesize(_ context.Context, _, output string) error {
	return os.WriteFile(output, []byte("RIFF"), 0600)
}
