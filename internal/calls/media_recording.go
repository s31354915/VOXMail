package calls

import (
	"context"
	"fmt"
	"github.com/voxmail/voxmail/internal/maintenance"
	"github.com/voxmail/voxmail/internal/process"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/voxmail/voxmail/internal/speech"
)

// The native baresip module is deliberately pinned to this media contract.
// Keeping the constants next to the raw-PCM readers makes a format drift fail
// closed instead of silently producing speed/pitch/channel corruption.
const (
	mediaSampleRate     int64 = 8000
	mediaChannels       int64 = 1
	mediaBytesPerSample       = 2 // signed little-endian 16-bit PCM
	defaultSpeechWindow       = 15 * time.Second
	maxSpeechWindow           = 60 * time.Second
)

type VoiceRecorder struct {
	Runtime *speech.Runtime
	Whisper speech.Whisper
	Binary  string
	Dir     string
	Window  time.Duration
}

func (r *VoiceRecorder) audioWindow() time.Duration {
	if r == nil || r.Window <= 0 || r.Window > 60*time.Second {
		return 30 * time.Second
	}
	return r.Window
}

func (r *VoiceRecorder) RecordAndTranscribe(ctx context.Context, rawPath string, offset int64) (string, error) {
	return r.recordAndTranscribe(ctx, rawPath, offset, nil)
}

// RecordAndTranscribeUntil captures until the caller signals stop, the
// configured maximum window expires, or the context is cancelled. A stop
// signal is a normal completion path; context cancellation remains an abort
// path for call teardown and the # navigation key.
func (r *VoiceRecorder) RecordAndTranscribeUntil(ctx context.Context, rawPath string, offset int64, stop <-chan struct{}) (string, error) {
	return r.recordAndTranscribe(ctx, rawPath, offset, stop)
}

func (r *VoiceRecorder) recordAndTranscribe(ctx context.Context, rawPath string, offset int64, stop <-chan struct{}) (string, error) {
	if r == nil || rawPath == "" {
		return "", fmt.Errorf("voice recorder is not configured")
	}
	window := r.Window
	if window <= 0 {
		window = defaultSpeechWindow
	}
	if window > maxSpeechWindow {
		window = maxSpeechWindow
	}
	timer := time.NewTimer(window)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-stop:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	info, err := os.Stat(rawPath)
	if err != nil {
		return "", err
	}
	if offset < 0 || offset > info.Size() {
		return "", fmt.Errorf("invalid recording offset")
	}
	// A writer can be observed between the two bytes of one PCM sample. Start
	// at the previous byte so the segment never begins with a half sample.
	offset -= offset % mediaBytesPerSample
	dir := r.Dir
	if dir == "" {
		dir = filepath.Dir(rawPath)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	raw, err := os.Open(rawPath)
	if err != nil {
		return "", err
	}
	defer raw.Close()
	if _, err := raw.Seek(offset, io.SeekStart); err != nil {
		return "", err
	}
	segment, releaseSegment, err := maintenance.CreateTemp(dir, ".recording-*.pcm")
	if err != nil {
		return "", err
	}
	defer releaseSegment()
	segmentPath := segment.Name()
	defer os.Remove(segmentPath)
	if _, err := io.Copy(segment, io.LimitReader(raw, pcmWindowBytes(window))); err != nil {
		segment.Close()
		return "", err
	}
	if err := segment.Close(); err != nil {
		return "", err
	}
	wav, releaseWav, err := maintenance.CreateTemp(dir, ".recording-*.wav")
	if err != nil {
		return "", err
	}
	defer releaseWav()
	wavPath := wav.Name()
	_ = wav.Close()
	defer os.Remove(wavPath)
	binary := r.Binary
	if binary == "" {
		binary = "ffmpeg"
	}
	convert := process.New(ctx, "ffmpeg", binary, process.ConversionTimeout, "-nostdin", "-f", "s16le", "-ar", "8000", "-ac", "1", "-i", segmentPath, "-ar", "16000", "-ac", "1", "-c:a", "pcm_s16le", "-y", wavPath)
	if output, err := convert.CombinedOutput(); err != nil {
		return "", fmt.Errorf("recording conversion: %w: %s", err, strings.TrimSpace(string(output)))
	}
	whisper := r.Whisper
	if r.Runtime != nil {
		if err := r.Runtime.WaitWhisper(ctx); err != nil {
			return "", err
		}
		whisper = r.Runtime.Whisper
	}
	return whisper.TranscribeAndRemove(ctx, wavPath)
}

// RecordAudio captures a bounded raw call segment and returns a normal WAV
// attachment. It deliberately does not run Whisper; callers who want speech
// composition use RecordAndTranscribe instead.
func (r *VoiceRecorder) RecordAudio(ctx context.Context, rawPath string, offset int64) ([]byte, error) {
	return r.RecordAudioUntil(ctx, rawPath, offset, nil)
}

// RecordAudioUntil treats stop as successful completion while cancellation
// remains owned by call teardown/navigation through conversion and cleanup.
func (r *VoiceRecorder) RecordAudioUntil(ctx context.Context, rawPath string, offset int64, stop <-chan struct{}) ([]byte, error) {
	if r == nil || rawPath == "" {
		return nil, fmt.Errorf("voice recorder is not configured")
	}
	window := r.audioWindow()
	timer := time.NewTimer(window)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-stop:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	info, err := os.Stat(rawPath)
	if err != nil {
		return nil, err
	}
	if offset < 0 || offset > info.Size() {
		return nil, fmt.Errorf("invalid recording offset")
	}
	offset -= offset % mediaBytesPerSample
	dir := r.Dir
	if dir == "" {
		dir = filepath.Dir(rawPath)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	segment, releaseSegment, err := maintenance.CreateTemp(dir, ".attachment-*.pcm")
	if err != nil {
		return nil, err
	}
	defer releaseSegment()
	segmentPath := segment.Name()
	defer os.Remove(segmentPath)
	input, err := os.Open(rawPath)
	if err != nil {
		segment.Close()
		return nil, err
	}
	if _, err := input.Seek(offset, io.SeekStart); err != nil {
		input.Close()
		segment.Close()
		return nil, err
	}
	_, copyErr := io.Copy(segment, io.LimitReader(input, pcmWindowBytes(window)))
	input.Close()
	if err := segment.Close(); err != nil {
		return nil, err
	}
	if copyErr != nil {
		return nil, copyErr
	}
	wav, releaseWav, err := maintenance.CreateTemp(dir, ".attachment-*.wav")
	if err != nil {
		return nil, err
	}
	defer releaseWav()
	wavPath := wav.Name()
	_ = wav.Close()
	defer os.Remove(wavPath)
	binary := r.Binary
	if binary == "" {
		binary = "ffmpeg"
	}
	convert := process.New(ctx, "ffmpeg", binary, process.ConversionTimeout, "-nostdin", "-f", "s16le", "-ar", "8000", "-ac", "1", "-i", segmentPath, "-ar", "8000", "-ac", "1", "-c:a", "pcm_s16le", "-y", wavPath)
	if output, err := convert.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("audio attachment conversion: %w: %s", err, strings.TrimSpace(string(output)))
	}
	converted, err := os.Open(wavPath)
	if err != nil {
		return nil, err
	}
	defer converted.Close()
	data, err := io.ReadAll(io.LimitReader(converted, (2<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 2<<20 {
		return nil, fmt.Errorf("audio attachment exceeds size limit")
	}
	return data, nil
}

// pcmWindowBytes returns an even byte count for the fixed native media
// contract. It avoids duration multiplication overflow and preserves whole
// samples even for sub-second windows.
func pcmWindowBytes(window time.Duration) int64 {
	if window <= 0 {
		return 0
	}
	seconds := int64(window / time.Second)
	nanos := int64(window % time.Second)
	bytes := seconds*mediaSampleRate*mediaChannels*mediaBytesPerSample +
		nanos*mediaSampleRate*mediaChannels*mediaBytesPerSample/int64(time.Second)
	return bytes - bytes%mediaBytesPerSample
}
