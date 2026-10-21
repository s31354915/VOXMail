// Provision checksum-verified real speech models for isolated acceptance runs.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/voxmail/voxmail/internal/speech"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: models DISPOSABLE_MODEL_DIRECTORY")
		os.Exit(2)
	}
	root, err := filepath.Abs(os.Args[1])
	if err != nil {
		fail(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	voices := filepath.Join(root, "voices")
	whisper := filepath.Join(root, "whisper", "ggml-base.en-q5_1.bin")
	if err := speech.Provision(ctx, voices, whisper); err != nil {
		fail(err)
	}
	piper, ok := speech.InstalledVoicePath(voices, "en_US-hfc_male-medium")
	if !ok {
		fail(fmt.Errorf("verified Piper installation is unavailable"))
	}
	piperRelative, err := filepath.Rel(root, piper)
	if err != nil {
		fail(err)
	}
	whisperRelative, err := filepath.Rel(root, whisper)
	if err != nil {
		fail(err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(map[string]string{
		"piper_model": filepath.ToSlash(piperRelative), "whisper_model": filepath.ToSlash(whisperRelative),
		"piper_sha256": speech.PiperModelSHA256, "whisper_sha256": speech.WhisperBaseENSHA256,
	}); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
