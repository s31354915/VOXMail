package speech

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

const (
	PiperModelURL    = "https://huggingface.co/rhasspy/piper-voices/resolve/v1.0.0/en/en_US/hfc_male/medium/en_US-hfc_male-medium.onnx?download=true"
	PiperConfigURL   = "https://huggingface.co/rhasspy/piper-voices/resolve/v1.0.0/en/en_US/hfc_male/medium/en_US-hfc_male-medium.onnx.json?download=true"
	WhisperBaseENURL = "https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-base.en.bin?download=true"
)

// Provision downloads only missing model files, writing each file atomically.
// It is opt-in because model downloads are large and should be visible in
// deployment logs.
func Provision(ctx context.Context, voiceDir, whisperPath string) error {
	if err := os.MkdirAll(voiceDir, 0700); err != nil {
		return err
	}
	voice := filepath.Join(voiceDir, "en_US-hfc_male-medium.onnx")
	if err := download(ctx, PiperModelURL, voice); err != nil {
		return err
	}
	if err := download(ctx, PiperConfigURL, voice+".json"); err != nil {
		return err
	}
	return download(ctx, WhisperBaseENURL, whisperPath)
}

func download(ctx context.Context, url, destination string) error {
	if info, err := os.Stat(destination); err == nil && info.Size() > 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return fmt.Errorf("download model: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download model: HTTP %s", resp.Status)
	}
	tmp, err := os.CreateTemp(filepath.Dir(destination), ".model-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err = io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, destination)
}
