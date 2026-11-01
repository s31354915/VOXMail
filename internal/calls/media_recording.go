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
	defaultSpeechWindow       = 120 * time.Second
	maxSpeechWindow           = 300 * time.Second
)

type VoiceRecorder struct {
	Runtime *speech.Runtime
	Whisper speech.Whisper
	Binary  string
	Dir     string
	Window  time.Duration
}

func (r *VoiceRecorder) audioWindow() time.Duration {
	if r == nil || r.Window <= 0 || r.Window > maxSpeechWindow {
		return defaultSpeechWindow
	}
	return r.Window
}

func (r *VoiceRecorder) speechWindow() time.Duration {
	if r == nil || r.Window <= 0 {
		return defaultSpeechWindow
	}
	if r.Window > maxSpeechWindow {
		return maxSpeechWindow
	}
	return r.Window
}

// RecordAndTranscribe captures and transcribes a segment. runtime is the
// call's activated speech runtime; STT readiness (and Whisper) are read from
// it, falling back to the recorder's own Runtime only when the caller has
// none. Passing an unactivated runtime fails with "speech runtime is not
// active" by design.
func (r *VoiceRecorder) RecordAndTranscribe(ctx context.Context, runtime *speech.Runtime, rawPath string, offset int64) (string, error) {
	return r.recordAndTranscribe(ctx, runtime, rawPath, offset, nil, nil)
}

// RecordAndTranscribeUntil captures until the caller signals stop, the
// configured maximum window expires, or the context is cancelled. A stop
// signal is a normal completion path; context cancellation remains an abort
// path for call teardown and the # navigation key.
func (r *VoiceRecorder) RecordAndTranscribeUntil(ctx context.Context, runtime *speech.Runtime, rawPath string, offset int64, stop <-chan struct{}) (string, error) {
	return r.recordAndTranscribe(ctx, runtime, rawPath, offset, stop, nil)
}

// RecordAndTranscribeUntilWithStatus calls afterCapture as soon as the captured
// audio is complete and before conversion/transcription begins. Audio played
// by the callback is excluded from the captured segment.
func (r *VoiceRecorder) RecordAndTranscribeUntilWithStatus(ctx context.Context, runtime *speech.Runtime, rawPath string, offset int64, stop <-chan struct{}, afterCapture func()) (string, error) {
	return r.recordAndTranscribe(ctx, runtime, rawPath, offset, stop, afterCapture)
}

// transcriptionChunkWindow bounds one STT segment: converting and
// transcribing finished audio overlaps the caller still speaking, so a long
// dictation finishes transcribing moments after the caller stops instead of
// starting from zero. The 300-second window cap keeps the in-flight chunk
// count below the channel buffers below, so the pipeline cannot deadlock.
const transcriptionChunkWindow = 20 * time.Second

func (r *VoiceRecorder) recordAndTranscribe(ctx context.Context, runtime *speech.Runtime, rawPath string, offset int64, stop <-chan struct{}, afterCapture func()) (string, error) {
	if r == nil || rawPath == "" {
		return "", fmt.Errorf("voice recorder is not configured")
	}
	window := r.speechWindow()
	sttRuntime := runtime
	if sttRuntime == nil {
		sttRuntime = r.Runtime
	}
	dir := r.Dir
	if dir == "" {
		dir = filepath.Dir(rawPath)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	if offset < 0 {
		return "", fmt.Errorf("invalid recording offset")
	}
	// Whisper warm-up stays lazy so the engine readies while the caller is
	// still speaking rather than delaying the first recording.
	resolveWhisper := func(ctx context.Context) (speech.Whisper, error) {
		if sttRuntime != nil {
			if err := sttRuntime.WaitWhisper(ctx); err != nil {
				return speech.Whisper{}, err
			}
			return sttRuntime.Whisper, nil
		}
		return r.Whisper, nil
	}
	if window <= transcriptionChunkWindow {
		return r.transcribeWholeWindow(ctx, resolveWhisper, rawPath, offset, dir, window, stop, afterCapture)
	}
	return r.transcribeOverlapped(ctx, resolveWhisper, rawPath, offset, dir, window, stop, afterCapture)
}

// transcribeWholeWindow is the single-segment path for recordings at or under
// one transcription chunk: wait for capture to end, then convert and
// transcribe exactly as before.
func (r *VoiceRecorder) transcribeWholeWindow(ctx context.Context, resolveWhisper func(context.Context) (speech.Whisper, error), rawPath string, offset int64, dir string, window time.Duration, stop <-chan struct{}, afterCapture func()) (string, error) {
	timer := time.NewTimer(window)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-stop:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	// The baresip writer buffers PCM before flushing it to the raw file.
	// Reading the segment immediately after the window ends can miss the
	// last bytes the caller actually spoke. Poll briefly for the file to
	// reach the expected length; a caller who stopped speaking early costs
	// at most this bounded wait.
	expected := offset + pcmWindowBytes(window)
	if rawInfo, statErr := os.Stat(rawPath); statErr == nil && rawInfo.Size() < expected {
		deadline := time.Now().Add(300 * time.Millisecond)
		for time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
			latest, latestErr := os.Stat(rawPath)
			if latestErr != nil || latest.Size() >= expected {
				break
			}
		}
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
	captureEnd := info.Size()
	if afterCapture != nil {
		afterCapture()
		if err := ctx.Err(); err != nil {
			return "", err
		}
	}
	segment, releaseSegment, err := maintenance.CreateTemp(dir, ".recording-*.pcm")
	if err != nil {
		return "", err
	}
	defer releaseSegment()
	segmentPath := segment.Name()
	defer os.Remove(segmentPath)
	raw, err := os.Open(rawPath)
	if err != nil {
		return "", err
	}
	if _, err := raw.Seek(offset, io.SeekStart); err != nil {
		raw.Close()
		return "", err
	}
	_, copyErr := io.Copy(segment, io.LimitReader(raw, captureEnd-offset))
	raw.Close()
	if err := segment.Close(); err != nil {
		return "", err
	}
	if copyErr != nil {
		return "", copyErr
	}
	whisper, err := resolveWhisper(ctx)
	if err != nil {
		return "", err
	}
	return r.transcribeSegment(ctx, whisper, segmentPath)
}

// transcribeSegment converts one captured PCM segment to the 16 kHz mono WAV
// the STT engine expects, transcribes it, and removes the intermediate file.
func (r *VoiceRecorder) transcribeSegment(ctx context.Context, whisper speech.Whisper, segmentPath string) (string, error) {
	dir := filepath.Dir(segmentPath)
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
	return whisper.TranscribeAndRemove(ctx, wavPath)
}

// transcribeOverlapped streams a long recording through Whisper in chunks
// while capture continues: each transcription chunk of captured audio is
// copied, converted and handed to the engine while the caller keeps speaking,
// so when the recording ends only the final chunk still needs transcribing.
func (r *VoiceRecorder) transcribeOverlapped(ctx context.Context, resolveWhisper func(context.Context) (speech.Whisper, error), rawPath string, offset int64, dir string, window time.Duration, stop <-chan struct{}, afterCapture func()) (string, error) {
	type chunkJob struct {
		index   int
		path    string
		release func()
	}
	type chunkResult struct {
		index int
		text  string
		err   error
	}
	// A 300-second window yields at most 15 chunks, so buffered channels of
	// 16 never block and the pipeline cannot deadlock.
	jobs := make(chan chunkJob, 16)
	results := make(chan chunkResult, 16)
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		var whisper speech.Whisper
		resolved := false
		var resolveErr error
		for job := range jobs {
			if ctx.Err() != nil {
				os.Remove(job.path)
				job.release()
				continue
			}
			if !resolved {
				whisper, resolveErr = resolveWhisper(ctx)
				resolved = true
			}
			if resolveErr != nil {
				os.Remove(job.path)
				job.release()
				select {
				case results <- chunkResult{index: job.index, err: resolveErr}:
					continue
				case <-ctx.Done():
					return
				}
			}
			text, err := r.transcribeSegment(ctx, whisper, job.path)
			os.Remove(job.path)
			job.release()
			select {
			case results <- chunkResult{index: job.index, text: text, err: err}:
			case <-ctx.Done():
				return
			}
		}
	}()
	cursor := offset - offset%mediaBytesPerSample
	chunkCount := 0
	stopped := false
	deadline := time.Now().Add(window)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		wait := transcriptionChunkWindow
		if wait > remaining {
			wait = remaining
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-stop:
			timer.Stop()
			stopped = true
		case <-ctx.Done():
			timer.Stop()
			close(jobs)
			<-workerDone
			return "", ctx.Err()
		}
		// Give the baresip writer a beat to flush the bytes for the audio
		// just spoken so a chunk boundary does not clip the last word.
		settleTarget := cursor + 2*mediaSampleRate*mediaBytesPerSample
		if info, statErr := os.Stat(rawPath); statErr == nil && info.Size() < settleTarget {
			settle := time.Now().Add(200 * time.Millisecond)
			for time.Now().Before(settle) {
				time.Sleep(50 * time.Millisecond)
				latest, latestErr := os.Stat(rawPath)
				if latestErr != nil || latest.Size() >= settleTarget {
					break
				}
			}
		}
		info, err := os.Stat(rawPath)
		if err != nil {
			close(jobs)
			<-workerDone
			return "", err
		}
		end := info.Size()
		if end > cursor {
			segment, releaseSegment, err := maintenance.CreateTemp(dir, ".recording-*.pcm")
			if err != nil {
				close(jobs)
				<-workerDone
				return "", err
			}
			raw, openErr := os.Open(rawPath)
			if openErr != nil {
				releaseSegment()
				os.Remove(segment.Name())
				close(jobs)
				<-workerDone
				return "", openErr
			}
			if _, seekErr := raw.Seek(cursor, io.SeekStart); seekErr != nil {
				raw.Close()
				segment.Close()
				releaseSegment()
				os.Remove(segment.Name())
				close(jobs)
				<-workerDone
				return "", seekErr
			}
			_, copyErr := io.Copy(segment, io.LimitReader(raw, end-cursor))
			raw.Close()
			segment.Close()
			if copyErr != nil {
				releaseSegment()
				os.Remove(segment.Name())
				close(jobs)
				<-workerDone
				return "", copyErr
			}
			jobs <- chunkJob{index: chunkCount, path: segment.Name(), release: releaseSegment}
			chunkCount++
			cursor = end - end%mediaBytesPerSample
		}
		if stopped || !time.Now().Before(deadline) {
			break
		}
	}
	close(jobs)
	<-workerDone
	if afterCapture != nil {
		afterCapture()
		if err := ctx.Err(); err != nil {
			return "", err
		}
	}
	if chunkCount == 0 {
		// Nothing was captured; an empty transcription matches the
		// single-segment path's behavior on silence.
		return "", nil
	}
	texts := make([]string, chunkCount)
	var firstErr error
	for range chunkCount {
		select {
		case result := <-results:
			texts[result.index] = result.text
			if result.err != nil && firstErr == nil {
				firstErr = result.err
			}
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if firstErr != nil {
		return "", firstErr
	}
	parts := make([]string, 0, chunkCount)
	for _, text := range texts {
		if trimmed := strings.TrimSpace(text); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	return strings.Join(parts, " "), nil
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
	data, err := io.ReadAll(io.LimitReader(converted, (8<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 8<<20 {
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
