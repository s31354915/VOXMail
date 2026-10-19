package calls

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/voxmail/voxmail/internal/maintenance"
	"github.com/voxmail/voxmail/internal/process"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/voxmail/voxmail/internal/speech"
	"github.com/voxmail/voxmail/internal/store"
)

// PromptPlayer turns short IVR prompts into 8 kHz signed PCM and writes them
// to the call's baresip FIFO. Calls are serialized per FIFO so prompts cannot
// overlap when a user barges in during a menu.
type PromptPlayer struct {
	Piper        speech.Piper
	Runtime      *speech.Runtime
	Pool         *speech.RuntimePool
	Store        *store.Store
	GreetingPath string
	Static       map[string]string
	StaticVoice  string
	Binary       string
	Dir          string
	staticMu     sync.RWMutex
	StaticByKey  map[string]map[string]string
	staticOrder  []string
	dynamicMu    sync.Mutex
	dynamicClean time.Time
	initMu       sync.Mutex
}

// maxStaticVoiceEntries bounds the per-voice static prompt index. Each entry
// holds a fully synthesized prompt set for one voice, speed, and model
// fingerprint; without a bound, switching voices or speeds over a long-lived
// process accumulated every old set forever.
const maxStaticVoiceEntries = 8

const (
	maxDynamicPromptFiles = 512
	maxDynamicPromptBytes = 128 << 20
	dynamicPromptMaxAge   = 7 * 24 * time.Hour
	maxDynamicPromptText  = 16 << 10
)

// PlayWithRuntime is kept as the small public playback primitive used by
// callers that do not have an owning user. User-scoped calls should use
// PlayWithRuntimeForUser so dynamic prompts can be reused safely.
func (p *PromptPlayer) PlayWithRuntime(ctx context.Context, runtime *speech.Runtime, fifo, text string) error {
	return p.PlayWithRuntimeForUser(ctx, runtime, "", fifo, text)
}

// PlayWithRuntimeForUser plays a prompt and caches dynamic speech by every
// value that can change its meaning or audio: user, voice, speed, text, and
// model checksum. Static prompts are handled by the existing manifest maps;
// this cache is only consulted for synthesized, user-scoped prompts.
func (p *PromptPlayer) PlayWithRuntimeForUser(ctx context.Context, runtime *speech.Runtime, userID, fifo, text string) error {
	if p == nil || fifo == "" || text == "" {
		return nil
	}
	p.initPlaybackDefaults(fifo)
	p.staticMu.RLock()
	staticVoice := p.StaticVoice
	staticAssets := p.Static
	staticMuAssets := staticAssets[text]
	if runtime != nil && p.StaticByKey != nil {
		staticMuAssets = ""
		if assets := p.StaticByKey[staticRuntimeKey(runtime)]; assets != nil {
			staticMuAssets = assets[text]
			staticVoice = ""
		}
	}
	p.staticMu.RUnlock()
	staticVoiceMatches := runtime == nil || staticVoice == "" || strings.TrimSuffix(filepath.Base(runtime.Piper.Model), filepath.Ext(runtime.Piper.Model)) == staticVoice
	if static := staticMuAssets; static != "" && staticVoiceMatches {
		if _, err := os.Stat(static); err == nil {
			return p.playWAVTail(ctx, fifo, static, pcmDrainTail)
		}
	}

	if runtime != nil && strings.TrimSpace(userID) != "" {
		if cached, ok := p.dynamicPromptPath(userID, runtime, text); ok {
			releaseCached := maintenance.Protect(cached)
			if _, err := os.Stat(cached); err == nil {
				err := p.playWAVTail(ctx, fifo, cached, pcmDrainTail)
				releaseCached()
				return err
			}
			releaseCached()
		}
	}
	if err := os.MkdirAll(p.Dir, 0700); err != nil {
		return err
	}
	tmp, releaseTmp, err := maintenance.CreateTemp(p.Dir, ".prompt-*.wav")
	if err != nil {
		return err
	}
	defer releaseTmp()
	wav := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(wav)
	if runtime != nil {
		if err := runtime.Synthesize(ctx, text, wav); err != nil {
			return err
		}
		if strings.TrimSpace(userID) != "" {
			if cached, ok := p.dynamicPromptPath(userID, runtime, text); ok {
				releaseCached := maintenance.Protect(cached)
				defer releaseCached()
				if err := p.activateDynamicPrompt(wav, cached); err == nil {
					p.pruneDynamicPrompts()
					return p.playWAVTail(ctx, fifo, cached, pcmDrainTail)
				}
			}
		}
		return p.playWAVTail(ctx, fifo, wav, pcmDrainTail)
	}
	if err := p.Piper.Synthesize(ctx, text, wav); err != nil {
		return err
	}
	return p.playWAVTail(ctx, fifo, wav, pcmDrainTail)
}

func (p *PromptPlayer) dynamicPromptPath(userID string, runtime *speech.Runtime, text string) (string, bool) {
	if runtime == nil || strings.TrimSpace(userID) == "" || strings.TrimSpace(runtime.Piper.Model) == "" || len(text) > maxDynamicPromptText {
		return "", false
	}
	checksum := runtime.Fingerprint
	var err error
	if checksum == "" {
		checksum, err = speech.ModelFingerprint(runtime.Piper.Model)
	}
	if err != nil || checksum == "" {
		return "", false
	}
	voice := strings.TrimSuffix(filepath.Base(runtime.Piper.Model), filepath.Ext(runtime.Piper.Model))
	// Include both the normalized text and its policy version. Existing prompt
	// files were generated from raw uppercase tokens and must not survive this
	// pronunciation fix merely because the source prompt is unchanged.
	key := strings.Join([]string{
		userID,
		voice,
		fmt.Sprint(runtime.Speed),
		checksum,
		fmt.Sprint(speech.SpeechNormalizationVersion),
		speech.NormalizeForSpeech(text),
	}, "\x00")
	name := fmt.Sprintf("%x.wav", sha256.Sum256([]byte(key)))
	return filepath.Join(p.Dir, "dynamic", name), true
}

func (p *PromptPlayer) pruneDynamicPrompts() {
	if p == nil {
		return
	}
	p.dynamicMu.Lock()
	defer p.dynamicMu.Unlock()
	if time.Since(p.dynamicClean) < time.Hour {
		return
	}
	p.dynamicClean = time.Now()
	root := filepath.Join(p.Dir, "dynamic")
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	type promptFile struct {
		path    string
		modTime time.Time
		size    int64
	}
	files := make([]promptFile, 0, len(entries))
	var total int64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".wav") {
			continue
		}
		info, statErr := entry.Info()
		if statErr != nil {
			continue
		}
		path := filepath.Join(root, entry.Name())
		if time.Since(info.ModTime()) > dynamicPromptMaxAge {
			if removed, _ := maintenance.RemoveIfUnowned(path); removed {
				continue
			}
		}
		files = append(files, promptFile{path: path, modTime: info.ModTime(), size: info.Size()})
		total += info.Size()
	}
	sort.Slice(files, func(i, j int) bool { return files[i].modTime.Before(files[j].modTime) })
	retained := len(files)
	for retained > maxDynamicPromptFiles || total > maxDynamicPromptBytes {
		if len(files) == 0 {
			break
		}
		oldest := files[0]
		files = files[1:]
		if removed, _ := maintenance.RemoveIfUnowned(oldest.path); removed {
			total -= oldest.size
			retained--
		}
	}
}

// PruneDynamicPrompts applies cache age and quota cleanup during startup and
// after synthesis. The maintenance worker independently expires idle cache
// in bounded periodic passes, including when no calls arrive.
func (p *PromptPlayer) PruneDynamicPrompts() {
	p.pruneDynamicPrompts()
}

func (p *PromptPlayer) activateDynamicPrompt(source, destination string) error {
	info, err := os.Stat(source)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 44 {
		return fmt.Errorf("generated dynamic prompt is invalid")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	stage, releaseStage, err := maintenance.CreateTemp(filepath.Dir(destination), ".dynamic-*.wav")
	if err != nil {
		return err
	}
	defer releaseStage()
	stagePath := stage.Name()
	defer os.Remove(stagePath)
	data, err := os.ReadFile(source)
	if err != nil {
		_ = stage.Close()
		return err
	}
	if _, err := stage.Write(data); err != nil {
		_ = stage.Close()
		return err
	}
	if err := stage.Chmod(0600); err != nil {
		_ = stage.Close()
		return err
	}
	if err := stage.Close(); err != nil {
		return err
	}
	if err := os.Rename(stagePath, destination); err != nil {
		if _, statErr := os.Stat(destination); statErr == nil {
			return nil
		}
		return err
	}
	return nil
}

type pcmLimitWriter struct {
	w          io.Writer
	n          int64
	max        int64
	overflowed bool
}

func (w *pcmLimitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.max-w.n {
		w.overflowed = true
		return 0, fmt.Errorf("decoded audio exceeds playback limit")
	}
	n, err := w.w.Write(p)
	w.n += int64(n)
	return n, err
}

func openPCMWriter(ctx context.Context, path string) (*os.File, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("audio output path is required")
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	isFIFO := info.Mode()&os.ModeNamedPipe != 0
	// The retry loop previously ran until the context expired, which for a
	// prompt playback is minutes. A FIFO with no reader that stays busy for
	// seconds is not going to recover on its own, and every retry second was
	// dead air for the caller. Bound the wait and fail the prompt.
	retryDeadline := time.Now().Add(5 * time.Second)
	for {
		file, openErr := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0600)
		if openErr == nil {
			// Keep the descriptor pollable (non-blocking): a full FIFO write
			// then parks in Go's poller until the reader drains, honoring
			// deadlines and cancellation, instead of surfacing EAGAIN as a
			// mid-prompt playback failure.

			return file, nil
		}
		var errno syscall.Errno
		if !isFIFO || (!errors.As(openErr, &errno) || (errno != syscall.ENXIO && errno != syscall.EAGAIN && errno != syscall.EWOULDBLOCK)) {
			return nil, openErr
		}
		if time.Now().After(retryDeadline) {
			return nil, fmt.Errorf("audio output never became writable: %w", openErr)
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (p *PromptPlayer) Activate() *speech.Lease {
	if p == nil || p.Runtime == nil {
		return nil
	}
	return p.Runtime.Activate()
}

func (p *PromptPlayer) ActivateForUser(userID string) (*speech.Runtime, *speech.Lease) {
	menuRuntime, menuLease, _, _ := p.ActivateForUserSpeeds(userID)
	return menuRuntime, menuLease
}

func (p *PromptPlayer) ActivateForUserSpeeds(userID string) (*speech.Runtime, *speech.Lease, *speech.Runtime, *speech.Lease) {
	if p == nil {
		return nil, nil, nil, nil
	}
	voice, menuSpeed, emailSpeed := "en_US-hfc_male-medium", 3, 2
	if p.Store != nil {
		if settings, err := p.Store.SpeechSettings(context.Background(), userID); err == nil {
			voice, menuSpeed, emailSpeed = settings.Voice, settings.MenuSpeed, settings.EmailSpeed
		}
	}
	if p.Pool != nil {
		menuRuntime, menuLease := p.Pool.Activate(voice, menuSpeed)
		emailRuntime, emailLease := p.Pool.Activate(voice, emailSpeed)
		return menuRuntime, menuLease, emailRuntime, emailLease
	}
	return p.Runtime, p.Activate(), p.Runtime, p.Activate()
}

func (p *PromptPlayer) Play(ctx context.Context, fifo, text string) error {
	return p.PlayWithRuntime(ctx, p.Runtime, fifo, text)
}

// SetStaticPrompts replaces the fixed-prompt index only after a complete
// regeneration has succeeded. The call player can therefore be updated while
// other calls are still reading the previous map.
func (p *PromptPlayer) SetStaticPrompts(assets map[string]string, voice string) {
	p.SetStaticPromptsForSpeed(assets, voice, 3)
}

func (p *PromptPlayer) SetStaticPromptsForSpeed(assets map[string]string, voice string, speed int, fingerprint ...string) {
	if p == nil {
		return
	}
	copyAssets := make(map[string]string, len(assets))
	for text, path := range assets {
		copyAssets[text] = path
	}
	p.staticMu.Lock()
	if p.StaticByKey == nil {
		p.StaticByKey = make(map[string]map[string]string)
	}
	if p.staticOrder == nil {
		p.staticOrder = make([]string, 0, maxStaticVoiceEntries)
	}
	key := voice + "|" + fmt.Sprint(speed)
	if len(fingerprint) > 0 {
		key += "|" + fingerprint[0]
	}
	if _, exists := p.StaticByKey[key]; !exists {
		p.staticOrder = append(p.staticOrder, key)
	}
	p.StaticByKey[key] = copyAssets
	for len(p.staticOrder) > maxStaticVoiceEntries {
		oldest := p.staticOrder[0]
		p.staticOrder = p.staticOrder[1:]
		delete(p.StaticByKey, oldest)
	}
	if p.Static == nil || (speed == 3 && (p.StaticVoice == "" || p.StaticVoice == voice)) {
		p.Static = copyAssets
		p.StaticVoice = voice
	}
	p.staticMu.Unlock()
}

// initPlaybackDefaults lazily configures the ffmpeg binary and prompt
// directory. Binary and Dir used to be written from concurrent playback
// goroutines without any lock, a data race the race detector flags and that
// could leave one goroutine reading a stale binary name.
func (p *PromptPlayer) initPlaybackDefaults(fifo string) {
	if p == nil {
		return
	}
	p.initMu.Lock()
	defer p.initMu.Unlock()
	if p.Binary == "" {
		p.Binary = "ffmpeg"
	}
	if p.Dir == "" {
		p.Dir = filepath.Dir(fifo)
	}
}

// playbackBinary reads the configured ffmpeg binary under the same lock that
// guards its lazy initialization.
func (p *PromptPlayer) playbackBinary(fifo string) string {
	if p == nil {
		return "ffmpeg"
	}
	p.initPlaybackDefaults(fifo)
	p.initMu.Lock()
	defer p.initMu.Unlock()
	if p.Binary == "" {
		return "ffmpeg"
	}
	return p.Binary
}

func staticRuntimeKey(runtime *speech.Runtime) string {
	if runtime == nil {
		return ""
	}
	voice := strings.TrimSuffix(filepath.Base(runtime.Piper.Model), filepath.Ext(runtime.Piper.Model))
	return voice + "|" + fmt.Sprint(runtime.Speed) + "|" + runtime.Fingerprint
}

func (p *PromptPlayer) PlayGreeting(ctx context.Context, fifo string) error {
	return p.PlayGreetingWithRuntime(ctx, nil, fifo)
}

func (p *PromptPlayer) PlayGreetingWithRuntime(ctx context.Context, runtime *speech.Runtime, fifo string) error {
	if p == nil {
		return fmt.Errorf("static greeting is not configured")
	}
	if runtime != nil {
		p.staticMu.RLock()
		var greeting string
		if assets := p.StaticByKey[staticRuntimeKey(runtime)]; assets != nil {
			greeting = assets[speech.StaticWelcomeText]
		}
		p.staticMu.RUnlock()
		if greeting != "" {
			if _, err := os.Stat(greeting); err == nil {
				return p.playWAVTail(ctx, fifo, greeting, pcmDrainTail)
			}
		}
		// A selected voice may not have an in-memory static index after restart.
		// Generate its greeting before resorting to the deployment backup.
		if err := p.PlayWithRuntime(ctx, runtime, fifo, speech.StaticWelcomeText); err == nil {
			return nil
		} else if ctx.Err() != nil || p.GreetingPath == "" {
			return err
		}
	}
	if p.GreetingPath == "" {
		return fmt.Errorf("static greeting is not configured")
	}
	return p.playWAVTail(ctx, fifo, p.GreetingPath, pcmDrainTail)
}

// PlayMedia converts an extracted audio or video attachment to the raw PCM
// format expected by the per-call baresip FIFO. Video is intentionally
// rendered audio-only; no video frames are ever sent to a phone call.
func (p *PromptPlayer) PlayMedia(ctx context.Context, fifo, input string) error {
	return p.playInput(ctx, fifo, input, pcmDrainTail)
}

func (p *PromptPlayer) playWAV(ctx context.Context, fifo, wav string) error {
	return p.playWAVTail(ctx, fifo, wav, pcmDrainTail)
}

// playWAVTail plays a wav with an explicit drain tail. Streamed body chunks
// use the shorter tail so chunks follow each other without a long pause.
func (p *PromptPlayer) playWAVTail(ctx context.Context, fifo, wav string, drainTail time.Duration) error {
	release := maintenance.Protect(wav)
	defer release()
	return p.playInput(ctx, fifo, wav, drainTail)
}

func (p *PromptPlayer) playInput(ctx context.Context, fifo, input string, drainTail time.Duration) error {
	if p == nil {
		return fmt.Errorf("prompt player is not configured")
	}
	binary := p.playbackBinary(fifo)
	const maxPlaybackDuration = process.PlaybackTimeout
	mediaCtx, cancel := context.WithTimeout(ctx, maxPlaybackDuration)
	defer cancel()
	pipe, err := openPCMWriter(mediaCtx, fifo)
	if err != nil {
		return err
	}
	defer pipe.Close()
	rawRead, rawWrite, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("prompt conversion: %w", err)
	}
	defer rawRead.Close()
	// Closing a pollable FIFO wakes both pending writes and process output
	// copying. Killing ffmpeg alone cannot wake a blocked Go output writer.
	stopClose := context.AfterFunc(mediaCtx, func() {
		_ = pipe.Close()
		_ = rawRead.Close()
	})
	defer stopClose()
	info, err := pipe.Stat()
	if err != nil {
		_ = rawWrite.Close()
		return err
	}
	var output io.Writer = pipe
	if info.Mode()&os.ModeNamedPipe != 0 {
		output = &pacedPCMWriter{ctx: mediaCtx, w: pipe}
	}
	writer := &pcmLimitWriter{w: output, max: 8_000 * 2 * 600}
	// Loudness normalization keeps quiet emails audible and loud ones from
	// clipping after the phone-band downscale, which callers heard as
	// "not clear" speech.
	cmd := process.New(mediaCtx, "ffmpeg", binary, 0, "-nostdin", "-i", input, "-vn", "-af", "loudnorm=I=-16:TP=-1.5:LRA=11", "-ar", "8000", "-ac", "1", "-f", "s16le", "pipe:1")
	// The converted PCM streams through a pipe we own instead of the pipe
	// exec wraps around an io.Writer. exec abandons whatever is still
	// buffered in its pipe once the child exits and WaitDelay (500 ms)
	// lapses, silently discarding the prompt's tail as "WaitDelay expired
	// before I/O complete"; callers heard that as menus cut off
	// mid-sentence. With a descriptor we own, exec hands it to the child
	// directly and the drain below runs to the last byte.
	cmd.Stdout = rawWrite
	streamDone := make(chan error, 1)
	go func() { streamDone <- copyConvertedPCM(mediaCtx, rawRead, writer) }()
	runErr := cmd.Run()
	// The child has exited: close our write end so the drain reaches EOF
	// once every buffered byte has been paced out to the call.
	_ = rawWrite.Close()
	streamErr := <-streamDone
	if mediaCtx.Err() != nil {
		return mediaCtx.Err()
	}
	// Reaching the playback cap is a normal stop, not a failure: the
	// listener heard the capped audio and should get the normal tail
	// drain instead of an error callers report as a failed prompt.
	if runErr != nil && !writer.overflowed {
		return fmt.Errorf("prompt conversion: %w", runErr)
	}
	if streamErr != nil && !writer.overflowed {
		return fmt.Errorf("prompt conversion: %w", streamErr)
	}
	if writer.n == 0 && !writer.overflowed {
		return fmt.Errorf("decoded audio is empty")
	}
	if info.Mode()&os.ModeNamedPipe != 0 {
		// A read-only observer lets the writer close (and flush the native
		// reader's final partial frame) while we inspect unread FIFO bytes.
		observer, err := os.OpenFile(fifo, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		defer observer.Close()
		if err := pipe.Close(); err != nil {
			return err
		}
		return drainPCM(mediaCtx, observer, drainTail)
	}
	return nil
}

// copyConvertedPCM drains the converter's raw output into the destination
// writer until EOF, a write error, or cancellation. The reads and paced
// writes happen in this goroutine after the child has exited, so no buffered
// byte is ever abandoned.
func copyConvertedPCM(ctx context.Context, src *os.File, dst io.Writer) error {
	buf := make([]byte, 32*1024)
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				return writeErr
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}
