package speech

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
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
	StaticWelcomeText  = "Welcome to VOXMail. Please enter your PIN, then press pound."
	StaticMainText     = "You are signed in. Press 1 for email, 2 for settings, or 3 for information and instructions."
)

type StaticPromptManifest struct {
	Version      int               `json:"version"`
	VoiceModel   string            `json:"voice_model"`
	ModelSHA256  string            `json:"model_sha256"`
	PromptSHA256 string            `json:"prompt_sha256"`
	WelcomeText  string            `json:"welcome_text"`
	MainText     string            `json:"main_text"`
	Assets       map[string]string `json:"assets,omitempty"`
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
		_, _ = io.WriteString(hash, texts[key])
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

// PrepareStaticPrompts keeps all fixed recordings in sync with the Piper
// model and IVR prompt text. A matching manifest returns without touching the
// model. If the model, prompt text, or asset set changes, every fixed prompt
// is generated in a private staging directory before activation.
func PrepareStaticPrompts(ctx context.Context, p Piper, greetingPath, mainPath, manifestPath string) (StaticPromptManifest, error) {
	texts := StaticPromptTexts()
	digest := promptDigest(texts)
	current, manifestErr := ReadStaticPromptManifest(manifestPath)
	if manifestErr == nil && safeStaticAssets(current.Assets) && current.Version >= 2 && current.PromptSHA256 == digest && current.WelcomeText == StaticWelcomeText && current.MainText == StaticMainText {
		voice := filepath.Base(p.Model)
		voice = strings.TrimSuffix(voice, filepath.Ext(voice))
		if voice == current.VoiceModel {
			complete := true
			for _, relative := range current.Assets {
				if _, err := os.Stat(filepath.Join(filepath.Dir(manifestPath), relative)); err != nil {
					complete = false
					break
				}
			}
			if complete && len(current.Assets) == len(texts) {
				if current.ModelSHA256 == "" {
					return current, nil
				}
				if _, err := os.Stat(p.Model); os.IsNotExist(err) {
					return current, nil
				}
				if modelDigest, err := ModelSHA256(p.Model); err == nil && modelDigest == current.ModelSHA256 {
					return current, nil
				}
			}
		}
	}
	if _, err := os.Stat(p.Model); err != nil {
		return current, fmt.Errorf("static prompt model unavailable: %w", err)
	}
	modelDigest, err := ModelSHA256(p.Model)
	if err != nil {
		return current, err
	}
	voice := strings.TrimSuffix(filepath.Base(p.Model), filepath.Ext(p.Model))
	manifest := StaticPromptManifest{Version: 2, VoiceModel: voice, ModelSHA256: modelDigest, PromptSHA256: promptDigest(texts), WelcomeText: StaticWelcomeText, MainText: StaticMainText, Assets: make(map[string]string, len(texts))}
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
			filename = filepath.Base(greetingPath)
		} else if key == "main" {
			filename = filepath.Base(mainPath)
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
	activeDir := filepath.Dir(manifestPath)
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
	return manifest, nil
}

// Runtime owns the expensive speech engines for the duration of one or more
// active calls. The first caller starts a warmup; additional callers share it.
// When the final lease is released, the warmup context is cancelled and the
// next caller gets a fresh lifecycle.
type Runtime struct {
	Piper   Piper
	Whisper Whisper
	Dir     string
	Speed   int

	mu      sync.Mutex
	refs    int
	ready   chan struct{}
	warmErr error
	cancel  context.CancelFunc
	worker  *piperWorker
	onZero  func()
}

type Lease struct {
	runtime *Runtime
	once    sync.Once
}

func NewRuntime(piper Piper, whisper Whisper, dir string) *Runtime {
	return &Runtime{Piper: piper, Whisper: whisper, Dir: dir, Speed: 3}
}

type RuntimePool struct {
	PiperBinary   string
	VoiceDir      string
	WhisperBinary string
	WhisperModel  string
	Dir           string
	mu            sync.Mutex
	runtimes      map[string]*Runtime
}

func NewRuntimePool(piperBinary, voiceDir, whisperBinary, whisperModel, dir string) *RuntimePool {
	return &RuntimePool{PiperBinary: piperBinary, VoiceDir: voiceDir, WhisperBinary: whisperBinary, WhisperModel: whisperModel, Dir: dir, runtimes: make(map[string]*Runtime)}
}

func (p *RuntimePool) Activate(voice string, speed int) (*Runtime, *Lease) {
	if p == nil {
		return nil, nil
	}
	model := voice
	if model == "" {
		model = "en_US-hfc_male-medium"
	}
	if !strings.HasSuffix(model, ".onnx") {
		model += ".onnx"
	}
	if !filepath.IsAbs(model) {
		model = filepath.Join(p.VoiceDir, model)
	}
	key := fmt.Sprintf("%s|%d", model, speed)
	p.mu.Lock()
	runtime := p.runtimes[key]
	if runtime == nil {
		runtime = NewRuntime(Piper{Binary: p.PiperBinary, Model: model, Extra: SpeedExtra(speed)}, Whisper{Binary: p.WhisperBinary, Model: p.WhisperModel}, filepath.Join(p.Dir, fmt.Sprintf("%x", sha256.Sum256([]byte(key)))))
		runtime.Speed = normalizeSpeed(speed)
		runtime.onZero = func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			runtime.mu.Lock()
			active := runtime.refs > 0
			runtime.mu.Unlock()
			if !active && p.runtimes[key] == runtime {
				delete(p.runtimes, key)
			}
		}
		p.runtimes[key] = runtime
	}
	p.mu.Unlock()
	return runtime, runtime.Activate()
}

func SpeedExtra(speed int) []string {
	speed = normalizeSpeed(speed)
	// Piper's length_scale is inverse speed; 3 is the neutral middle setting.
	return []string{"--length_scale", fmt.Sprintf("%.2f", 1.25-0.125*float64(speed))}
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
		r.ready = make(chan struct{})
		r.warmErr = nil
		warmCtx, cancel := context.WithCancel(context.Background())
		r.cancel = cancel
		ready := r.ready
		go r.warm(warmCtx, ready)
	}
	r.refs++
	r.mu.Unlock()
	return &Lease{runtime: r}
}

func (r *Runtime) warm(ctx context.Context, ready chan struct{}) {
	var wg sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex
	var worker *piperWorker
	record := func(err error) {
		if err != nil {
			errMu.Lock()
			if firstErr == nil {
				firstErr = err
			}
			errMu.Unlock()
		}
	}
	wg.Add(2)
	go func() {
		defer wg.Done()
		w, err := startPiperWorker(ctx, r.Piper)
		if err != nil {
			record(err)
			return
		}
		r.mu.Lock()
		installed := r.ready == ready && r.worker == nil
		if installed {
			r.worker = w
		}
		r.mu.Unlock()
		if !installed {
			w.close()
			return
		}
		worker = w
		warmDir := r.Dir
		if warmDir == "" {
			warmDir = os.TempDir()
		}
		if err := os.MkdirAll(warmDir, 0700); err != nil {
			record(err)
			return
		}
		file, err := os.CreateTemp(warmDir, ".piper-warm-*.wav")
		if err != nil {
			record(err)
			return
		}
		path := file.Name()
		_ = file.Close()
		if err := worker.synthesize(ctx, "VOXMail ready.", path); err != nil {
			record(err)
		}
		_ = os.Remove(path)
	}()
	go func() {
		defer wg.Done()
		record(r.Whisper.Warm(ctx, r.Dir))
	}()
	wg.Wait()
	r.mu.Lock()
	if r.ready == ready {
		r.warmErr = firstErr
		close(ready)
		r.mu.Unlock()
		return
	}
	// This warm belongs to a superseded generation. Tear down only the worker
	// we installed, never a worker installed by a newer warm.
	if r.worker == worker {
		r.worker = nil
	}
	r.mu.Unlock()
	if worker != nil {
		worker.close()
	}
}

func (r *Runtime) Wait(ctx context.Context) error {
	if r == nil {
		return fmt.Errorf("speech runtime is not configured")
	}
	r.mu.Lock()
	ready := r.ready
	r.mu.Unlock()
	if ready == nil {
		return fmt.Errorf("speech runtime is not active")
	}
	select {
	case <-ready:
		r.mu.Lock()
		err := r.warmErr
		r.mu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Runtime) Synthesize(ctx context.Context, text, output string) error {
	if err := r.Wait(ctx); err != nil {
		return err
	}
	r.mu.Lock()
	worker := r.worker
	r.mu.Unlock()
	if worker == nil {
		return fmt.Errorf("piper worker is not active")
	}
	return worker.synthesize(ctx, text, output)
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
			r.ready = nil
			r.warmErr = nil
			worker := r.worker
			onZero := r.onZero
			r.worker = nil
			r.mu.Unlock()
			if worker != nil {
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
	file, err := os.CreateTemp(dir, ".whisper-warm-*.wav")
	if err != nil {
		return err
	}
	path := file.Name()
	defer os.Remove(path)
	if err := writeSilentWAV(file, 8000); err != nil {
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
	if err != nil && err.Error() != "whisper returned empty transcription" {
		return err
	}
	return nil
}

func PrepareGreeting(ctx context.Context, p Piper, path string) error {
	if path == "" {
		return fmt.Errorf("greeting path is required")
	}
	if info, err := os.Stat(path); err == nil && info.Size() > 44 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".greeting-*.wav")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(tmpPath)
	if err := p.Synthesize(ctx, "Welcome to VOXMail. Please enter your PIN, then press pound.", tmpPath); err != nil {
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
