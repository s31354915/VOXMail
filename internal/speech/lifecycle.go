package speech

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/voxmail/voxmail/internal/maintenance"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/voxmail/voxmail/internal/ivr"
)

const (
	BundledPromptVoice = "en_US-hfc_male-medium"
	// One recording owns the welcome and PIN instruction. Keeping them in
	// one synthesis avoids a second conversion, drain gap, and volume change.
	StaticWelcomeText           = "Welcome to VOXMail. Please enter your PIN, then press pound."
	StaticMainText              = "You are signed in. " + ivr.MainMenuPrompt
	staticPromptManifestVersion = 7
)

type StaticPromptManifest struct {
	Version       int               `json:"version"`
	VoiceModel    string            `json:"voice_model"`
	ConfigSHA256  string            `json:"config_sha256"`
	OptionsSHA256 string            `json:"options_sha256"`
	ModelSHA256   string            `json:"model_sha256"`
	PromptSHA256  string            `json:"prompt_sha256"`
	WelcomeText   string            `json:"welcome_text"`
	MainText      string            `json:"main_text"`
	Assets        map[string]string `json:"assets,omitempty"`
}

// StaticPromptTexts returns every fixed prompt that can be spoken without
// account, folder, message, or contact data. The keys are stable manifest
// identities; the text is included in the manifest digest so changing a menu
// prompt automatically invalidates the generated asset set.
func StaticPromptTexts() map[string]string {
	texts := map[string]string{
		"welcome": StaticWelcomeText,
		"main":    StaticMainText,
	}
	for _, spec := range ivr.States() {
		if strings.TrimSpace(spec.Prompt) != "" {
			texts["state-"+string(spec.State)] = spec.Prompt
		}
	}
	return texts
}

func promptDigest(texts map[string]string) string {
	keys := make([]string, 0, len(texts))
	for key := range texts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	hash := sha256.New()
	for _, key := range keys {
		_, _ = io.WriteString(hash, key)
		_, _ = io.WriteString(hash, "\x00")
		_, _ = io.WriteString(hash, NormalizeForSpeech(texts[key]))
		_, _ = io.WriteString(hash, "\x00")
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}

func ReadStaticPromptManifest(path string) (StaticPromptManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return StaticPromptManifest{}, err
	}
	var manifest StaticPromptManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return StaticPromptManifest{}, err
	}
	return manifest, nil
}

func safeStaticAssetPath(path string) bool {
	path = filepath.ToSlash(strings.TrimSpace(path))
	return path != "" && path != "." && path != ".." && !strings.HasPrefix(path, "/") && !strings.Contains(path, "/../") && !strings.HasPrefix(path, "../") && !strings.Contains(path, "\\") && filepath.Base(path) == path
}

func safeStaticAssets(assets map[string]string) bool {
	for _, path := range assets {
		if !safeStaticAssetPath(path) {
			return false
		}
	}
	return true
}

func ModelSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

// ModelFingerprint identifies both files used by Piper. Missing sidecars are
// represented explicitly so adding one invalidates existing runtime identities.
func ModelFingerprint(path string) (string, error) {
	model, err := ModelSHA256(path)
	if err != nil {
		return "", err
	}
	config, err := ModelSHA256(path + ".json")
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(model+"|"+config))), nil
}

// Copy a canonical prompt to an operator-selected location atomically.
func syncPromptDestination(source, destination string) error {
	if filepath.Clean(source) == filepath.Clean(destination) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(destination), ".prompt-copy-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), destination)
}

// PrepareStaticPrompts keeps all fixed recordings in sync with the Piper
// model and IVR prompt text. A matching manifest returns without touching the
// model. If the model, prompt text, or asset set changes, every fixed prompt
// is generated in a private staging directory before activation.
func PrepareStaticPrompts(ctx context.Context, p Piper, greetingPath, mainPath, manifestPath string) (StaticPromptManifest, error) {
	activeDir := filepath.Dir(manifestPath)
	canonicalGreeting := filepath.Join(activeDir, "welcome.wav")
	canonicalMain := filepath.Join(activeDir, "main-menu.wav")
	if filepath.Clean(greetingPath) == filepath.Clean(mainPath) || filepath.Clean(greetingPath) == filepath.Clean(manifestPath) || filepath.Clean(mainPath) == filepath.Clean(manifestPath) || filepath.Clean(greetingPath) == filepath.Clean(canonicalMain) || filepath.Clean(mainPath) == filepath.Clean(canonicalGreeting) {
		return StaticPromptManifest{}, fmt.Errorf("prompt destinations must be distinct")
	}
	syncDestinations := func() error {
		if err := syncPromptDestination(canonicalGreeting, greetingPath); err != nil {
			return err
		}
		return syncPromptDestination(canonicalMain, mainPath)
	}
	texts := StaticPromptTexts()
	digest := promptDigest(texts)
	options, _ := json.Marshal(p.Extra)
	optionsDigest := fmt.Sprintf("%x", sha256.Sum256(options))
	configDigest, configErr := ModelSHA256(p.Model + ".json")
	current, manifestErr := ReadStaticPromptManifest(manifestPath)
	if manifestErr == nil && safeStaticAssets(current.Assets) && current.Version == staticPromptManifestVersion && configErr == nil && current.ConfigSHA256 == configDigest && current.OptionsSHA256 == optionsDigest && current.PromptSHA256 == digest && current.WelcomeText == StaticWelcomeText && current.MainText == StaticMainText {
		voice := filepath.Base(p.Model)
		voice = strings.TrimSuffix(voice, filepath.Ext(voice))
		if voice == current.VoiceModel {
			complete := true
			for key := range texts {
				relative, exists := current.Assets[key]
				if !exists || !ValidWAV(filepath.Join(filepath.Dir(manifestPath), relative)) {
					complete = false
					break
				}
			}
			if complete && len(current.Assets) == len(texts) {
				if modelDigest, err := ModelSHA256(p.Model); err == nil && modelDigest == current.ModelSHA256 {
					return current, syncDestinations()
				}
			}
		}
	}
	if _, err := os.Stat(p.Model); err != nil {
		return current, fmt.Errorf("static prompt model unavailable: %w", err)
	}
	if configErr != nil {
		return current, fmt.Errorf("static prompt model configuration unavailable: %w", configErr)
	}
	modelDigest, err := ModelSHA256(p.Model)
	if err != nil {
		return current, err
	}
	voice := strings.TrimSuffix(filepath.Base(p.Model), filepath.Ext(p.Model))
	manifest := StaticPromptManifest{Version: staticPromptManifestVersion, ConfigSHA256: configDigest, OptionsSHA256: optionsDigest, VoiceModel: voice, ModelSHA256: modelDigest, PromptSHA256: digest, WelcomeText: StaticWelcomeText, MainText: StaticMainText, Assets: make(map[string]string, len(texts))}
	if err := os.MkdirAll(filepath.Dir(greetingPath), 0700); err != nil {
		return current, err
	}
	if err := os.MkdirAll(filepath.Dir(mainPath), 0700); err != nil {
		return current, err
	}
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0700); err != nil {
		return current, err
	}
	stage, err := os.MkdirTemp(filepath.Dir(manifestPath), ".static-prompts-")
	if err != nil {
		return current, err
	}
	defer os.RemoveAll(stage)
	keys := make([]string, 0, len(texts))
	for key := range texts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		filename := "static-" + fmt.Sprintf("%x", sha256.Sum256([]byte(key+"\x00"+texts[key])))[:16] + ".wav"
		if key == "welcome" {
			filename = "welcome.wav"
		} else if key == "main" {
			filename = "main-menu.wav"
		}
		path := filepath.Join(stage, filename)
		if err := p.Synthesize(ctx, texts[key], path); err != nil {
			return current, fmt.Errorf("static prompt %s: %w", key, err)
		}
		manifest.Assets[key] = filename
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return current, err
	}
	manifestStage := filepath.Join(stage, "static-prompts.json")
	if err := os.WriteFile(manifestStage, append(data, '\n'), 0600); err != nil {
		return current, err
	}
	backup, err := os.MkdirTemp(activeDir, ".static-prompts-backup-")
	if err != nil {
		return current, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(backup)
		}
	}()
	oldPaths := make(map[string]struct{}, len(current.Assets)+len(manifest.Assets)+1)
	for _, relative := range current.Assets {
		if safeStaticAssetPath(relative) {
			oldPaths[relative] = struct{}{}
		}
	}
	for _, relative := range manifest.Assets {
		oldPaths[relative] = struct{}{}
	}
	oldPaths[filepath.Base(manifestPath)] = struct{}{}
	restoreBackups := func() {
		entries, _ := os.ReadDir(backup)
		for _, entry := range entries {
			_ = os.Rename(filepath.Join(backup, entry.Name()), filepath.Join(activeDir, entry.Name()))
		}
	}
	for relative := range oldPaths {
		target := filepath.Join(activeDir, relative)
		if _, err := os.Stat(target); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			restoreBackups()
			return current, err
		}
		if err := os.Rename(target, filepath.Join(backup, relative)); err != nil {
			restoreBackups()
			return current, err
		}
	}
	restore := func() {
		for _, relative := range manifest.Assets {
			_ = os.Remove(filepath.Join(activeDir, relative))
		}
		_ = os.Remove(manifestPath)
		restoreBackups()
	}
	for _, relative := range manifest.Assets {
		if err := os.Rename(filepath.Join(stage, relative), filepath.Join(activeDir, relative)); err != nil {
			restore()
			return current, err
		}
	}
	if err := os.Rename(manifestStage, manifestPath); err != nil {
		restore()
		return current, err
	}
	committed = true
	_ = os.RemoveAll(backup)
	return manifest, syncDestinations()
}

type capabilityReady struct {
	done   chan struct{}
	err    error
	closed bool
}

func newCapabilityReady() *capabilityReady {
	return &capabilityReady{done: make(chan struct{})}
}

func (r *capabilityReady) finish(err error) {
	if r == nil || r.closed {
		return
	}
	r.err = err
	r.closed = true
	close(r.done)
}

// Runtime owns the expensive speech engines for the duration of one or more
// active calls. Piper/TTS and Whisper/STT warm independently: a broken STT
// installation must not prevent fixed or dynamic TTS menus from working.
type Runtime struct {
	Piper       Piper
	Whisper     Whisper
	Dir         string
	Speed       int
	Fingerprint string
	// Log, when nil, falls back to the default slog logger for the
	// lifecycle diagnostics below.
	Log *slog.Logger

	mu       sync.Mutex
	refs     int
	ready    *capabilityReady
	ttsReady *capabilityReady
	sttReady *capabilityReady
	// sttShared points at the pool's single Whisper warm state for
	// pool-managed runtimes; standalone runtimes keep nil and warm their
	// own Whisper. The shared state is warmed once on first pool
	// activation: the STT model is independent of voice and speed, and the
	// whisper CLI is invoked per transcription, so there is nothing
	// resident to unload between calls.
	sttShared *capabilityReady
	warmErr   error
	ttsErr    error
	sttErr    error
	cancel    context.CancelFunc
	worker    piperSynthesizer
	// workerRelease releases a pool-shared per-model worker when this
	// runtime leaves the pool. Standalone runtures keep nil and own their
	// worker outright.
	workerRelease func()
	// residentEnabled and residentPython govern the worker a standalone
	// runtime creates when no pool pre-assigned one.
	residentEnabled bool
	residentPython  string
	onZero          func()
}

type Lease struct {
	runtime *Runtime
	once    sync.Once
}

func NewRuntime(piper Piper, whisper Whisper, dir string) *Runtime {
	fingerprint, _ := ModelFingerprint(piper.Model)
	return &Runtime{Piper: piper, Whisper: whisper, Dir: dir, Speed: 3, Fingerprint: fingerprint, residentEnabled: true, residentPython: "python3"}
}

// pooledWorker is one TTS engine shared by every speech runtime built on
// the same voice model. With the resident engine, the model loads once per
// voice instead of once per voice-speed combination or per prompt.
type pooledWorker struct {
	synth    piperSynthesizer
	voice    string
	resident bool
	refs     int
}

type RuntimePool struct {
	PiperBinary   string
	VoiceDir      string
	WhisperBinary string
	WhisperModel  string
	Dir           string
	// PiperPython is the interpreter that runs the resident Piper worker;
	// Resident enables that engine (one-shot CLI otherwise).
	PiperPython string
	Resident    bool
	mu          sync.Mutex
	runtimes    map[string]*Runtime
	workers     map[string]*pooledWorker
	pinned      map[string]*Lease
	// sttWarm is the pool's single shared Whisper warm state, warmed on
	// first activation and shared by every runtime regardless of voice or
	// speed.
	sttWarm *capabilityReady
}

func NewRuntimePool(piperBinary, voiceDir, whisperBinary, whisperModel, dir, piperPython string, resident bool) *RuntimePool {
	return &RuntimePool{PiperBinary: piperBinary, VoiceDir: voiceDir, WhisperBinary: whisperBinary, WhisperModel: whisperModel, Dir: dir, PiperPython: piperPython, Resident: resident, runtimes: make(map[string]*Runtime)}
}

// sharedWhisperWarmLocked returns the pool's shared Whisper warm state,
// starting the warm-up on first use. Caller holds p.mu.
func (p *RuntimePool) sharedWhisperWarmLocked() *capabilityReady {
	if p.sttWarm != nil {
		return p.sttWarm
	}
	warm := newCapabilityReady()
	p.sttWarm = warm
	whisper := Whisper{Binary: p.WhisperBinary, Model: p.WhisperModel}
	dir := p.Dir
	go func() {
		err := whisper.Warm(context.Background(), dir)
		// A failed warm-up clears the slot so the next activation retries
		// instead of caching the failure for the pool's lifetime.
		p.mu.Lock()
		if p.sttWarm == warm && err != nil {
			p.sttWarm = nil
		}
		p.mu.Unlock()
		warm.finish(err)
	}()
	return warm
}

func (p *RuntimePool) Activate(voice string, speed int) (*Runtime, *Lease) {
	if p == nil {
		return nil, nil
	}
	model := voice
	if model == "" {
		model = "en_US-hfc_male-medium"
	}
	originalModel := model
	if !strings.HasSuffix(model, ".onnx") {
		model += ".onnx"
	}
	if !filepath.IsAbs(model) {
		model = filepath.Join(p.VoiceDir, model)
		if resolved, ok := InstalledVoicePath(p.VoiceDir, strings.TrimSuffix(filepath.Base(model), filepath.Ext(model))); ok {
			model = resolved
		}
	}
	modelDigest, _ := ModelFingerprint(model)
	if modelDigest == "" {
		modelDigest = "unavailable"
	}
	normalizedSpeed := normalizeSpeed(speed)
	key := fmt.Sprintf("%s|%s|%d", strings.TrimSpace(originalModel), modelDigest, normalizedSpeed)
	p.mu.Lock()
	runtime := p.runtimes[key]
	created := runtime == nil
	if created {
		runtime = NewRuntime(Piper{Binary: p.PiperBinary, Model: model, Extra: SpeedExtra(normalizedSpeed)}, Whisper{Binary: p.WhisperBinary, Model: p.WhisperModel}, filepath.Join(p.Dir, fmt.Sprintf("%x", sha256.Sum256([]byte(key)))))
		runtime.Speed = normalizedSpeed
		runtime.sttShared = p.sharedWhisperWarmLocked()
		runtime.onZero = func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			runtime.mu.Lock()
			active := runtime.refs > 0
			runtime.mu.Unlock()
			if !active && p.runtimes[key] == runtime {
				delete(p.runtimes, key)
				if release := runtime.workerRelease; release != nil {
					runtime.workerRelease = nil
					release()
				}
			}
		}
		p.runtimes[key] = runtime
	}
	p.mu.Unlock()
	if created {
		// The per-model TTS engine is shared across every speed of the same
		// voice: the resident engine applies the speech rate per request, so
		// one loaded model serves the whole voice. workerFor takes the pool
		// lock, so it runs outside the locked block above.
		if worker, release := p.workerFor(model, modelDigest, poolVoiceKey(originalModel)); worker != nil {
			runtime.mu.Lock()
			runtime.worker = worker
			runtime.workerRelease = release
			runtime.mu.Unlock()
		} else {
			runtime.residentEnabled = p.Resident
			runtime.residentPython = p.PiperPython
		}
	}
	return runtime, runtime.Activate()
}

func SpeedExtra(speed int) []string {
	speed = normalizeSpeed(speed)
	// Piper's length_scale is inverse speech rate: speed 3 is Piper's own
	// natural pace (length_scale 1.0), 1 is slowest and 5 is fastest.
	return []string{"--length_scale", fmt.Sprintf("%.2f", 1.375-0.125*float64(speed))}
}

func normalizeSpeed(speed int) int {
	if speed < 1 || speed > 5 {
		return 3
	}
	return speed
}

func (r *Runtime) Acquire(ctx context.Context) (*Lease, error) {
	if r == nil {
		return nil, fmt.Errorf("speech runtime is not configured")
	}
	lease := r.Activate()
	if err := r.Wait(ctx); err != nil {
		lease.Release()
		return nil, err
	}
	return lease, nil
}

func (r *Runtime) Activate() *Lease {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	if r.refs == 0 {
		r.ready = newCapabilityReady()
		r.ttsReady = newCapabilityReady()
		if r.sttShared != nil {
			// Pooled runtimes wait on the pool's shared Whisper warm-up
			// instead of running a duplicate per voice-speed key.
			r.sttReady = r.sttShared
		} else {
			r.sttReady = newCapabilityReady()
		}
		r.warmErr = nil
		r.ttsErr = nil
		r.sttErr = nil
		warmCtx, cancel := context.WithCancel(context.Background())
		r.cancel = cancel
		ready := r.ready
		go r.warm(warmCtx, ready)
	}
	r.refs++
	r.mu.Unlock()
	return &Lease{runtime: r}
}

func (r *Runtime) warm(ctx context.Context, ready *capabilityReady) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		w, owned, err := r.acquireWorker(ctx)
		if err != nil {
			r.mu.Lock()
			if r.ready == ready {
				r.ttsErr = err
				r.ttsReady.finish(err)
			}
			r.mu.Unlock()
			return
		}
		// A pool-shared worker outlives this runtime and must never be
		// closed from warm-up; only a worker warm-up created itself is
		// disposable.
		discard := func() {
			if owned {
				w.close()
			}
		}
		r.mu.Lock()
		installed := r.ready == ready && (r.worker == nil || r.worker == w)
		if installed && r.worker == nil {
			r.worker = w
		}
		r.mu.Unlock()
		if !installed {
			discard()
			return
		}
		warmDir := r.Dir
		if warmDir == "" {
			warmDir = os.TempDir()
		}
		if err := os.MkdirAll(warmDir, 0700); err != nil {
			discard()
			r.mu.Lock()
			if r.ready == ready && r.worker == w {
				if owned {
					r.worker = nil
				}
				r.ttsErr = err
				r.ttsReady.finish(err)
			}
			r.mu.Unlock()
			return
		}
		file, releaseFile, err := maintenance.CreateTemp(warmDir, ".piper-warm-*.wav")
		if err != nil {
			discard()
			r.mu.Lock()
			if r.ready == ready && r.worker == w {
				if owned {
					r.worker = nil
				}
				r.ttsErr = err
				r.ttsReady.finish(err)
			}
			r.mu.Unlock()
			return
		}
		defer releaseFile()
		path := file.Name()
		_ = file.Close()
		warmErr := w.synthesize(ctx, "VOXMail ready.", path, r.Speed)
		_ = os.Remove(path)
		if warmErr != nil {
			discard()
		}
		r.mu.Lock()
		if r.ready == ready {
			if warmErr != nil && owned && r.worker == w {
				r.worker = nil
			}
			r.ttsErr = warmErr
			r.ttsReady.finish(warmErr)
		}
		r.mu.Unlock()
	}()
	go func() {
		defer wg.Done()
		var warmErr error
		if r.sttShared != nil {
			// Pooled runtimes share the pool's Whisper warm-up: wait for
			// it rather than running a duplicate, and never finish the
			// shared state from here.
			select {
			case <-r.sttShared.done:
				warmErr = r.sttShared.err
			case <-ctx.Done():
				warmErr = ctx.Err()
			}
		} else {
			warmErr = r.Whisper.Warm(ctx, r.Dir)
		}
		r.mu.Lock()
		if r.ready == ready {
			r.sttErr = warmErr
			// A standalone runtime owns its STT state and must finish it;
			// a pooled runtime's sttReady is the pool's shared state, which
			// the pool's own warm-up finishes.
			if r.sttReady != nil && r.sttReady != r.sttShared {
				r.sttReady.finish(warmErr)
			}
		}
		r.mu.Unlock()
	}()
	wg.Wait()
	r.mu.Lock()
	if r.ready == ready {
		r.warmErr = combineCapabilityErrors(r.ttsErr, r.sttErr)
		ready.finish(r.warmErr)
	}
	r.mu.Unlock()
}

func combineCapabilityErrors(ttsErr, sttErr error) error {
	if ttsErr == nil && sttErr == nil {
		return nil
	}
	if ttsErr != nil && sttErr != nil {
		return fmt.Errorf("speech warm-up failed: tts: %v; whisper: %w", ttsErr, sttErr)
	}
	if ttsErr != nil {
		return fmt.Errorf("speech TTS warm-up failed: %w", ttsErr)
	}
	return fmt.Errorf("speech Whisper warm-up failed: %w", sttErr)
}

// acquireWorker returns the TTS worker this warm-up should use. A pool-
// managed runtime reuses the pool's shared per-model worker, which outlives
// individual runtimes and must never be closed here (owned=false). A
// standalone runtime prefers the resident engine and falls back to the
// one-shot CLI when the resident environment is unavailable.
func (r *Runtime) acquireWorker(ctx context.Context) (piperSynthesizer, bool, error) {
	r.mu.Lock()
	if r.worker != nil {
		worker := r.worker
		r.mu.Unlock()
		return worker, false, nil
	}
	r.mu.Unlock()
	if r.residentEnabled {
		if worker := newResidentWorker(r.residentPython, r.Piper); worker != nil {
			return worker, true, nil
		}
	}
	worker, err := startPiperWorker(ctx, r.Piper)
	if err != nil {
		return nil, false, err
	}
	return worker, true, nil
}

func (r *Runtime) Wait(ctx context.Context) error {
	return r.waitCapability(ctx, func(runtime *Runtime) *capabilityReady { return runtime.ready })
}

// WaitTTS waits only for Piper readiness.
func (r *Runtime) WaitTTS(ctx context.Context) error {
	return r.waitCapability(ctx, func(runtime *Runtime) *capabilityReady { return runtime.ttsReady })
}

// WaitWhisper waits only for Whisper readiness.
func (r *Runtime) WaitWhisper(ctx context.Context) error {
	err := r.waitCapability(ctx, func(runtime *Runtime) *capabilityReady { return runtime.sttReady })
	if err != nil {
		r.log().Warn("whisper is unavailable", "error", err)
	}
	return err
}

// log returns the runtime's logger, or the default when none is set.
func (r *Runtime) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

func (r *Runtime) waitCapability(ctx context.Context, selectReady func(*Runtime) *capabilityReady) error {
	if r == nil {
		return fmt.Errorf("speech runtime is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	r.mu.Lock()
	ready := selectReady(r)
	r.mu.Unlock()
	if ready == nil {
		return fmt.Errorf("speech runtime is not active")
	}
	select {
	case <-ready.done:
		r.mu.Lock()
		err := ready.err
		r.mu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Runtime) Synthesize(ctx context.Context, text, output string) error {
	if err := r.WaitTTS(ctx); err != nil {
		return err
	}
	speed := r.Speed
	r.mu.Lock()
	worker := r.worker
	r.mu.Unlock()
	if worker == nil {
		return fmt.Errorf("piper worker is not active")
	}
	return worker.synthesize(ctx, text, output, speed)
}

func (l *Lease) Release() {
	if l == nil || l.runtime == nil {
		return
	}
	l.once.Do(func() {
		r := l.runtime
		r.mu.Lock()
		if r.refs > 0 {
			r.refs--
		}
		if r.refs == 0 {
			if r.cancel != nil {
				r.cancel()
			}
			r.cancel = nil
			ready := r.ready
			ttsReady := r.ttsReady
			sttReady := r.sttReady
			sttShared := r.sttShared
			r.ready = nil
			r.ttsReady = nil
			r.sttReady = nil
			r.warmErr = nil
			r.ttsErr = nil
			r.sttErr = nil
			if ready != nil {
				ready.finish(context.Canceled)
			}
			if ttsReady != nil {
				ttsReady.finish(context.Canceled)
			}
			if sttReady != nil && sttReady != sttShared {
				sttReady.finish(context.Canceled)
			}
			worker := r.worker
			onZero := r.onZero
			release := r.workerRelease
			r.worker = nil
			r.mu.Unlock()
			r.log().Warn("speech runtime references dropped to zero; tearing down engines", "fingerprint", r.Fingerprint)
			// A pool-shared worker is released through the pool's registry,
			// which keeps the process alive while any runtime still uses it;
			// closing it here would kill a model another call is using.
			if release == nil && worker != nil {
				worker.close()
			}
			if onZero != nil {
				onZero()
			}
			return
		}
		r.mu.Unlock()
	})
}

func (w Whisper) Warm(ctx context.Context, dir string) error {
	if w.Binary == "" {
		w.Binary = "whisper-cli"
	}
	if w.Model == "" {
		return fmt.Errorf("whisper model is required")
	}
	if _, err := exec.LookPath(w.Binary); err != nil {
		return err
	}
	if info, err := os.Stat(w.Model); err != nil || info.Size() == 0 {
		if err == nil {
			err = fmt.Errorf("model is empty")
		}
		return err
	}
	if dir == "" {
		dir = os.TempDir()
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	file, releaseFile, err := maintenance.CreateTemp(dir, ".whisper-warm-*.wav")
	if err != nil {
		return err
	}
	defer releaseFile()
	path := file.Name()
	defer os.Remove(path)
	// whisper-cli rejects any other rate ("WAV file must be 16 kHz"), so
	// the warm-up fixture has to match the model's expected input format.
	if err := writeSilentWAV(file, 16000); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	// An empty transcription is expected; the purpose is to load and validate
	// the model while the caller is hearing the static greeting. Other
	// failures must abort the lease so the call never discovers a broken STT
	// engine halfway through composition.
	_, err = w.Transcribe(ctx, path)
	if err != nil && !errors.Is(err, errWhisperEmptyTranscription) {
		return err
	}
	return nil
}

func PrepareGreeting(ctx context.Context, p Piper, path string) error {
	if path == "" {
		return fmt.Errorf("greeting path is required")
	}
	if ValidWAV(path) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp, releaseTmp, err := maintenance.CreateTemp(filepath.Dir(path), ".greeting-*.wav")
	if err != nil {
		return err
	}
	defer releaseTmp()
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(tmpPath)
	if err := p.Synthesize(ctx, StaticWelcomeText, tmpPath); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func writeSilentWAV(file *os.File, sampleRate int) error {
	const seconds = 1
	dataSize := uint32(sampleRate * seconds * 2)
	if _, err := file.Write([]byte("RIFF")); err != nil {
		return err
	}
	for _, value := range []uint32{36 + dataSize} {
		if err := binary.Write(file, binary.LittleEndian, value); err != nil {
			return err
		}
	}
	if _, err := file.Write([]byte("WAVEfmt ")); err != nil {
		return err
	}
	if err := binary.Write(file, binary.LittleEndian, uint32(16)); err != nil {
		return err
	}
	if err := binary.Write(file, binary.LittleEndian, uint16(1)); err != nil {
		return err
	}
	if err := binary.Write(file, binary.LittleEndian, uint16(1)); err != nil {
		return err
	}
	if err := binary.Write(file, binary.LittleEndian, uint32(sampleRate)); err != nil {
		return err
	}
	if err := binary.Write(file, binary.LittleEndian, uint32(sampleRate*2)); err != nil {
		return err
	}
	if err := binary.Write(file, binary.LittleEndian, uint16(2)); err != nil {
		return err
	}
	if err := binary.Write(file, binary.LittleEndian, uint16(16)); err != nil {
		return err
	}
	if _, err := file.Write([]byte("data")); err != nil {
		return err
	}
	if err := binary.Write(file, binary.LittleEndian, dataSize); err != nil {
		return err
	}
	_, err := file.Write(make([]byte, dataSize))
	return err
}

// workerFor returns the per-model TTS engine for a runtime, creating it on
// first use and refcounting it while any runtime on that model exists. The
// engine outlives individual runtimes so overlapping calls at different
// speech rates share one loaded model.
func (p *RuntimePool) workerFor(model, digest, voice string) (piperSynthesizer, func()) {
	key := digest
	if key == "" || key == "unavailable" {
		key = model
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.workers == nil {
		p.workers = make(map[string]*pooledWorker)
	}
	if entry, ok := p.workers[key]; ok {
		entry.refs++
		return entry.synth, p.releaseWorkerLocked(key)
	}
	piper := Piper{Binary: p.PiperBinary, Model: model}
	var synth piperSynthesizer
	resident := false
	if p.Resident {
		if worker := newResidentWorker(p.PiperPython, piper); worker != nil {
			synth = worker
			resident = true
		}
	}
	if synth == nil {
		worker, err := startPiperWorker(context.Background(), piper)
		if err != nil {
			return nil, nil
		}
		synth = worker
	}
	entry := &pooledWorker{synth: synth, voice: voice, resident: resident, refs: 1}
	p.workers[key] = entry
	return synth, p.releaseWorkerLocked(key)
}

// releaseWorkerLocked returns a closure that drops one runtime's reference
// on the shared engine and closes it when the last reference goes away. The
// caller must hold the pool lock: workerFor runs it while building an
// entry, and the runtime's onZero callback holds the pool lock when it
// fires.
func (p *RuntimePool) releaseWorkerLocked(key string) func() {
	return func() {
		entry, ok := p.workers[key]
		if !ok {
			return
		}
		if entry.refs > 0 {
			entry.refs--
		}
		if entry.refs > 0 {
			return
		}
		delete(p.workers, key)
		if entry.synth != nil {
			entry.synth.close()
		}
	}
}

// KeepWarm pins a voice's model in memory: its resident engine stays loaded
// across calls until ReleaseKeepWarm unpins it or the process exits. The
// default voice is pinned at startup.
func (p *RuntimePool) KeepWarm(voice string, speed int) *Runtime {
	if p == nil {
		return nil
	}
	runtime, lease := p.Activate(voice, speed)
	if runtime == nil || lease == nil {
		return runtime
	}
	key := poolVoiceKey(voice)
	p.mu.Lock()
	if p.pinned == nil {
		p.pinned = make(map[string]*Lease)
	}
	if existing, ok := p.pinned[key]; ok {
		p.mu.Unlock()
		if existing != lease {
			lease.Release()
		}
		return runtime
	}
	p.pinned[key] = lease
	p.mu.Unlock()
	return runtime
}

// ReleaseKeepWarm unpins a voice's model. The engine unloads when the last
// call using it ends.
func (p *RuntimePool) ReleaseKeepWarm(voice string) bool {
	if p == nil {
		return false
	}
	key := poolVoiceKey(voice)
	p.mu.Lock()
	var lease *Lease
	if p.pinned != nil {
		lease = p.pinned[key]
		delete(p.pinned, key)
	}
	p.mu.Unlock()
	if lease != nil {
		lease.Release()
	}
	return lease != nil
}

// ResidentVoices lists the voices whose models are currently loaded in a
// resident engine, for the web console's warmth display.
func (p *RuntimePool) ResidentVoices() []string {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	names := make([]string, 0, len(p.workers))
	for _, entry := range p.workers {
		if entry.refs > 0 && entry.resident && entry.synth != nil && entry.synth.alive() {
			names = append(names, entry.voice)
		}
	}
	sort.Strings(names)
	return names
}

// poolVoiceKey normalizes a voice reference to the display name used by the
// keep-warm registry and the resident-voice list.
func poolVoiceKey(voice string) string {
	name := strings.TrimSpace(voice)
	if name == "" {
		return BundledPromptVoice
	}
	return strings.TrimSuffix(filepath.Base(name), filepath.Ext(name))
}
