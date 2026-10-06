package calls

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
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
	dynamicMu    sync.Mutex
	dynamicClean time.Time
}

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
	if p.Binary == "" {
		p.Binary = "ffmpeg"
	}
	if p.Dir == "" {
		p.Dir = filepath.Dir(fifo)
	}
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
			return p.playWAV(ctx, fifo, static)
		}
	}

	if runtime != nil && strings.TrimSpace(userID) != "" {
		if cached, ok := p.dynamicPromptPath(userID, runtime, text); ok {
			if _, err := os.Stat(cached); err == nil {
				return p.playWAV(ctx, fifo, cached)
			}
		}
	}
	if err := os.MkdirAll(p.Dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(p.Dir, ".prompt-*.wav")
	if err != nil {
		return err
	}
	wav := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(wav)
	if runtime != nil {
		if err := runtime.Synthesize(ctx, text, wav); err != nil {
			return err
		}
		if strings.TrimSpace(userID) != "" {
			if cached, ok := p.dynamicPromptPath(userID, runtime, text); ok {
				if err := p.activateDynamicPrompt(wav, cached); err == nil {
					p.pruneDynamicPrompts()
					return p.playWAV(ctx, fifo, cached)
				}
			}
		}
		return p.playWAV(ctx, fifo, wav)
	}
	if err := p.Piper.Synthesize(ctx, text, wav); err != nil {
		return err
	}
	return p.playWAV(ctx, fifo, wav)
}

func (p *PromptPlayer) dynamicPromptPath(userID string, runtime *speech.Runtime, text string) (string, bool) {
	if runtime == nil || strings.TrimSpace(userID) == "" || strings.TrimSpace(runtime.Piper.Model) == "" || len(text) > maxDynamicPromptText {
		return "", false
	}
	checksum, err := speech.ModelSHA256(runtime.Piper.Model)
	if err != nil || checksum == "" {
		return "", false
	}
	voice := strings.TrimSuffix(filepath.Base(runtime.Piper.Model), filepath.Ext(runtime.Piper.Model))
	key := strings.Join([]string{userID, voice, fmt.Sprint(runtime.Speed), checksum, text}, "\x00")
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
			_ = os.Remove(path)
			continue
		}
		files = append(files, promptFile{path: path, modTime: info.ModTime(), size: info.Size()})
		total += info.Size()
	}
	sort.Slice(files, func(i, j int) bool { return files[i].modTime.Before(files[j].modTime) })
	for len(files) > maxDynamicPromptFiles || total > maxDynamicPromptBytes {
		if len(files) == 0 {
			break
		}
		oldest := files[0]
		files = files[1:]
		total -= oldest.size
		_ = os.Remove(oldest.path)
	}
}

// PruneDynamicPrompts performs the bounded cache cleanup during startup as
// well as after synthesis, so a deployment that receives no calls still
// removes expired generated speech.
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
	stage, err := os.CreateTemp(filepath.Dir(destination), ".dynamic-*.wav")
	if err != nil {
		return err
	}
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
	w   io.Writer
	n   int64
	max int64
}

func (w *pcmLimitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.max-w.n {
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
	for {
		file, openErr := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0600)
		if openErr == nil {
			if isFIFO {
				// Once a reader is present, restore blocking writes so a slow
				// baresip callback does not turn ordinary audio into EAGAIN.
				if nonblockErr := syscall.SetNonblock(int(file.Fd()), false); nonblockErr != nil {
					_ = file.Close()
					return nil, nonblockErr
				}
			}
			return file, nil
		}
		var errno syscall.Errno
		if !isFIFO || (!errors.As(openErr, &errno) || (errno != syscall.ENXIO && errno != syscall.EAGAIN && errno != syscall.EWOULDBLOCK)) {
			return nil, openErr
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

func (p *PromptPlayer) SetStaticPromptsForSpeed(assets map[string]string, voice string, speed int) {
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
	p.StaticByKey[voice+"|"+fmt.Sprint(speed)] = copyAssets
	if p.Static == nil || (speed == 3 && (p.StaticVoice == "" || p.StaticVoice == voice)) {
		p.Static = copyAssets
		p.StaticVoice = voice
	}
	p.staticMu.Unlock()
}

func staticRuntimeKey(runtime *speech.Runtime) string {
	if runtime == nil {
		return ""
	}
	voice := strings.TrimSuffix(filepath.Base(runtime.Piper.Model), filepath.Ext(runtime.Piper.Model))
	return voice + "|" + fmt.Sprint(runtime.Speed)
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
				return p.playWAV(ctx, fifo, greeting)
			}
		}
	}
	if p.GreetingPath == "" {
		return fmt.Errorf("static greeting is not configured")
	}
	return p.playWAV(ctx, fifo, p.GreetingPath)
}

// PlayMedia converts an extracted audio or video attachment to the raw PCM
// format expected by the per-call baresip FIFO. Video is intentionally
// rendered audio-only; no video frames are ever sent to a phone call.
func (p *PromptPlayer) PlayMedia(ctx context.Context, fifo, input string) error {
	return p.playInput(ctx, fifo, input)
}

func (p *PromptPlayer) playWAV(ctx context.Context, fifo, wav string) error {
	return p.playInput(ctx, fifo, wav)
}

func (p *PromptPlayer) playInput(ctx context.Context, fifo, input string) error {
	if p == nil {
		return fmt.Errorf("prompt player is not configured")
	}
	binary := p.Binary
	if binary == "" {
		binary = "ffmpeg"
	}
	const maxPlaybackDuration = 10 * time.Minute
	mediaCtx, cancel := context.WithTimeout(ctx, maxPlaybackDuration)
	defer cancel()
	pipe, err := openPCMWriter(mediaCtx, fifo)
	if err != nil {
		return err
	}
	defer pipe.Close()
	cmd := exec.CommandContext(mediaCtx, binary, "-nostdin", "-i", input, "-vn", "-ar", "8000", "-ac", "1", "-f", "s16le", "pipe:1")
	cmd.Stdout = &pcmLimitWriter{w: pipe, max: 8_000 * 2 * 600}
	if err := cmd.Run(); err != nil {
		if mediaCtx.Err() != nil {
			return mediaCtx.Err()
		}
		return fmt.Errorf("prompt conversion: %w", err)
	}
	return nil
}
