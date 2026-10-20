package speech

import (
	"context"
	"errors"
	"fmt"
	"github.com/voxmail/voxmail/internal/maintenance"
	"github.com/voxmail/voxmail/internal/operation"
	"github.com/voxmail/voxmail/internal/process"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Piper struct {
	Binary string
	Model  string
	Extra  []string
}

func (p Piper) Synthesize(ctx context.Context, text, output string) error {
	return synthesizePiper(ctx, p, text, output)
}

var errWhisperEmptyTranscription = errors.New("whisper returned empty transcription")

func (p Piper) Warm(ctx context.Context, dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, releaseTmp, err := maintenance.CreateTemp(dir, ".warm-*.wav")
	if err != nil {
		return err
	}
	defer releaseTmp()
	path := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(path)
	warmCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return p.Synthesize(warmCtx, "VOXMail ready.", path)
}

type Whisper struct{ Binary, Model string }

// TranscribeAndRemove enforces the privacy boundary for caller recordings:
// once recognition returns, the raw source is removed whether recognition
// succeeds or fails.
func (w Whisper) TranscribeAndRemove(ctx context.Context, wav string) (text string, err error) {
	defer func() {
		removeErr := os.Remove(wav)
		if err == nil && removeErr != nil && !os.IsNotExist(removeErr) {
			err = fmt.Errorf("remove raw recording: %w", removeErr)
		}
	}()
	return w.Transcribe(ctx, wav)
}

const (
	// whisperPrompt biases decoding toward English so auto-detection cannot
	// drift to a random language on telephone hiss. Short, so it consumes
	// only a few of the half-context prompt tokens whisper-cli allows.
	whisperPrompt = "English voicemail transcription."
	// whisperNoSpeechThreshold raises whisper's no-speech threshold above the
	// 0.60 default: voicemail speech is close-mic'd and loud, so line hiss
	// during pauses should decode as silence rather than hallucinated text.
	whisperNoSpeechThreshold = "0.80"
)

func (w Whisper) Transcribe(ctx context.Context, wav string) (_ string, retErr error) {
	stage, category := "settings", operation.Configuration
	defer func() { retErr = operation.Wrap("whisper", stage, category, retErr) }()
	if wav == "" {
		return "", fmt.Errorf("audio file is required")
	}
	if w.Binary == "" {
		w.Binary = "whisper-cli"
	}
	if w.Model == "" {
		return "", fmt.Errorf("whisper model is required")
	}
	// Telephone audio misbehaves in whisper's defaults: auto-detected
	// language drifts to Welsh or Yiddish on line hiss, timestamps leak into
	// stdout for Piper to read aloud, and quiet audio loops the same phrase
	// until the context fills. Pin English, strip timestamps, disable
	// temperature fallback, tighten the no-speech threshold, and suppress
	// non-speech tokens. The wav stays the last argument: the test stubs read
	// it to know which file to fabricate.
	args := []string{
		"-m", w.Model,
		"-l", "en",
		"--no-timestamps",
		"--no-fallback",
		"--no-speech-thold", whisperNoSpeechThreshold,
		"--suppress-nst",
		"--prompt", whisperPrompt,
		"--no-prints",
		"-f", wav,
	}
	stage, category = "transcribe", operation.Failed
	command := process.New(ctx, "whisper", w.Binary, process.WhisperTimeout, args...)
	stdout := process.NewBuffer(process.MaxOutput)
	command.Stdout = stdout
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("whisper: %w: %s", err, strings.TrimSpace(command.StderrText()))
	}
	if stdout.Truncated() {
		return "", operation.Wrap("whisper", "output", operation.ResourceLimit, fmt.Errorf("whisper transcription exceeds output limit"))
	}
	text := strings.TrimSpace(stdout.String())
	if text == "" {
		return "", operation.Wrap("whisper", "output", operation.Protocol, errWhisperEmptyTranscription)
	}
	return text, nil
}

func safeOutput(path string) error {
	if path == "" || filepath.Base(path) == "." || filepath.Base(path) == ".." {
		return fmt.Errorf("invalid audio output path")
	}
	if strings.ContainsRune(path, '\x00') {
		return fmt.Errorf("invalid audio output path")
	}
	return nil
}
