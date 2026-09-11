package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/voxmail/voxmail/internal/alerts"
	"github.com/voxmail/voxmail/internal/calls"
	"github.com/voxmail/voxmail/internal/config"
	"github.com/voxmail/voxmail/internal/mailindex"
	"github.com/voxmail/voxmail/internal/mailsync"
	"github.com/voxmail/voxmail/internal/secret"
	"github.com/voxmail/voxmail/internal/sip"
	"github.com/voxmail/voxmail/internal/speech"
	"github.com/voxmail/voxmail/internal/store"
	"github.com/voxmail/voxmail/internal/web"
)

type voiceActivator struct {
	VoiceDir     string
	PiperBinary  string
	PromptDir    string
	GreetingPath string
	MainPath     string
	ManifestPath string
	Player       *calls.PromptPlayer
}

func (v *voiceActivator) ActivateVoice(ctx context.Context, name string, menuSpeed, emailSpeed int) error {
	voice, ok := speech.ResolveVoice(v.VoiceDir, name)
	if !ok {
		return errors.New("voice model is not installed or not in the trusted catalog")
	}
	model := filepath.Join(v.VoiceDir, voice.Voice+".onnx")
	config := model + ".json"
	if voice.ModelURL != "" {
		// Trusted catalog models are downloaded only through the pinned,
		// checksum-verified installer. Locally installed models are never
		// replaced or downloaded as a side effect of selecting them.
		if err := speech.InstallVoice(ctx, v.VoiceDir, voice.Voice); err != nil {
			return err
		}
	} else if !speech.IsInstalledVoice(v.VoiceDir, voice.Voice) {
		return errors.New("local voice model is not installed")
	}
	if !speech.ValidPiperConfig(config) {
		return errors.New("voice sidecar is missing after installation")
	}
	if err := (speech.Piper{Binary: v.PiperBinary, Model: model}).Warm(ctx, filepath.Join(v.PromptDir, "warm")); err != nil {
		return err
	}
	seenSpeeds := map[int]bool{}
	for _, speed := range []int{menuSpeed, emailSpeed} {
		if speed < 1 || speed > 5 {
			speed = 3
		}
		if seenSpeeds[speed] {
			continue
		}
		seenSpeeds[speed] = true
		cacheDir := filepath.Join(v.PromptDir, "static", voice.Voice, fmt.Sprint(speed))
		manifest, err := speech.PrepareStaticPrompts(ctx, speech.Piper{Binary: v.PiperBinary, Model: model, Extra: speech.SpeedExtra(speed)}, filepath.Join(cacheDir, "welcome.wav"), filepath.Join(cacheDir, "main-menu.wav"), filepath.Join(cacheDir, "static-prompts.json"))
		if err != nil {
			return err
		}
		texts := speech.StaticPromptTexts()
		assets := make(map[string]string, len(manifest.Assets))
		for key, relative := range manifest.Assets {
			if text, exists := texts[key]; exists {
				path := filepath.Join(cacheDir, relative)
				if info, statErr := os.Stat(path); statErr == nil && info.Size() > 44 {
					assets[text] = path
				}
			}
		}
		if len(assets) == 0 {
			return errors.New("voice prompt generation produced no usable assets")
		}
		if v.Player != nil {
			v.Player.SetStaticPromptsForSpeed(assets, manifest.VoiceModel, speed)
		}
	}
	return nil
}

// PreviewVoice synthesizes a short sample from an installed model. Preview
// never downloads a model; installation remains an explicit administrator
// operation in the web console.
func (v *voiceActivator) PreviewVoice(ctx context.Context, name string, speed int) ([]byte, error) {
	voice, ok := speech.ResolveVoice(v.VoiceDir, name)
	if !ok || !speech.IsInstalledVoice(v.VoiceDir, voice.Voice) {
		return nil, errors.New("voice model is not installed")
	}
	if speed < 1 || speed > 5 {
		speed = 3
	}
	dir := filepath.Join(v.PromptDir, "previews")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(dir, ".preview-*.wav")
	if err != nil {
		return nil, err
	}
	path := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	defer os.Remove(path)
	model := filepath.Join(v.VoiceDir, voice.Voice+".onnx")
	if err := (speech.Piper{Binary: v.PiperBinary, Model: model, Extra: speech.SpeedExtra(speed)}).Synthesize(ctx, "This is a VOXMail voice preview.", path); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := config.Load()
	if err != nil {
		log.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	secrets, err := secret.New(cfg.EncryptionKey)
	if err != nil {
		log.Error("cannot initialize secret store", "error", err)
		os.Exit(1)
	}
	for _, dir := range []string{cfg.DataDir, filepath.Dir(cfg.DBPath), filepath.Dir(cfg.ControlSocket), filepath.Join(cfg.DataDir, "run", "voxmail"), filepath.Join(cfg.DataDir, "run", "speech"), filepath.Join(cfg.DataDir, "logs"), cfg.VoiceDir, cfg.RecordingsDir, filepath.Dir(cfg.GreetingPath)} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			log.Error("cannot create data directory", "path", dir, "error", err)
			os.Exit(1)
		}
	}
	if provision, _ := strconv.ParseBool(os.Getenv("VOXMAIL_PROVISION_MODELS")); provision {
		modelContext, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		if err := speech.Provision(modelContext, cfg.VoiceDir, cfg.STTModel); err != nil {
			cancel()
			log.Error("model provisioning failed", "error", err)
			os.Exit(1)
		}
		cancel()
	}
	promptDir := filepath.Join(cfg.DataDir, "prompts")
	mainMenuPath := filepath.Join(promptDir, "main-menu.wav")
	manifestPath := filepath.Join(promptDir, "static-prompts.json")
	promptContext, promptCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	staticManifest, promptErr := speech.PrepareStaticPrompts(promptContext, speech.Piper{Binary: cfg.PiperBinary, Model: cfg.PiperModel}, cfg.GreetingPath, mainMenuPath, manifestPath)
	promptCancel()
	if promptErr != nil {
		log.Warn("static prompts unavailable; only existing recordings will be used", "error", promptErr)
	}
	staticPrompts := make(map[string]string)
	staticTexts := speech.StaticPromptTexts()
	for key, relative := range staticManifest.Assets {
		if text, ok := staticTexts[key]; ok {
			path := filepath.Join(promptDir, relative)
			if info, statErr := os.Stat(path); statErr == nil && info.Size() > 44 {
				staticPrompts[text] = path
			}
		}
	}
	// Older deployments have a version-one manifest containing only the main
	// menu. Keep that shipped asset usable until the next successful model
	// regeneration upgrades the manifest.
	if len(staticPrompts) == 0 {
		if info, statErr := os.Stat(mainMenuPath); statErr == nil && info.Size() > 44 {
			staticPrompts[speech.StaticMainText] = mainMenuPath
		}
	}
	db, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Error("cannot open database", "error", err)
		os.Exit(1)
	}

	sipBridge := &sip.Baresip{
		Binary:        cfg.BaresipBinary,
		ConfigDir:     cfg.BaresipConfig,
		ControlSocket: cfg.ControlSocket,
		AudioDir:      filepath.Join(cfg.DataDir, "run", "voxmail"),
		LogPath:       filepath.Join(cfg.DataDir, "logs", "baresip.log"),
		MaxCalls:      cfg.MaxCalls,
		Store:         db,
		Secrets:       secrets,
		Log:           log,
	}
	speechRuntime := speech.NewRuntime(
		speech.Piper{Binary: cfg.PiperBinary, Model: cfg.PiperModel},
		speech.Whisper{Binary: cfg.STTBinary, Model: cfg.STTModel},
		filepath.Join(cfg.DataDir, "run", "speech"),
	)
	speechPool := speech.NewRuntimePool(
		cfg.PiperBinary, cfg.VoiceDir, cfg.STTBinary, cfg.STTModel,
		filepath.Join(cfg.DataDir, "run", "speech"),
	)
	promptPlayer := &calls.PromptPlayer{
		Piper:        speech.Piper{Binary: cfg.PiperBinary, Model: cfg.PiperModel},
		Runtime:      speechRuntime,
		Pool:         speechPool,
		Store:        db,
		GreetingPath: cfg.GreetingPath,
		Static:       staticPrompts,
		StaticVoice:  staticManifest.VoiceModel,
		Dir:          promptDir,
	}
	bridgeService := &calls.Service{
		Socket:   cfg.ControlSocket,
		Store:    db,
		Log:      log,
		MaxCalls: cfg.MaxCalls,
		DataRoot: cfg.DataDir,
		Media:    promptPlayer,
		Recorder: &calls.VoiceRecorder{
			Runtime: speechRuntime,
			Whisper: speech.Whisper{Binary: cfg.STTBinary, Model: cfg.STTModel},
			Dir:     cfg.RecordingsDir,
			Window:  15 * time.Second,
		},
		Secrets: secrets,
	}
	alertService := &alerts.Service{Store: db, Bridge: bridgeService, Log: log}
	voiceService := &voiceActivator{VoiceDir: cfg.VoiceDir, PiperBinary: cfg.PiperBinary, PromptDir: promptDir, GreetingPath: cfg.GreetingPath, MainPath: mainMenuPath, ManifestPath: manifestPath, Player: promptPlayer}
	var syncService *mailsync.Service
	webApp := &web.Server{Store: db, Secrets: secrets, Log: log, SIP: sipBridge, Alerts: alertService, Voice: voiceService, Preview: voiceService, Calls: bridgeService, DataRoot: cfg.DataDir, VoiceDir: cfg.VoiceDir, Ready: func(ctx context.Context) error {
		if sipBridge.Enabled() && !bridgeService.Healthy() {
			return errors.New("call bridge not connected")
		}
		if syncService == nil || !syncService.Healthy() {
			return errors.New("mail synchronization worker is not running")
		}
		if sipBridge.Enabled() {
			if info, statErr := os.Stat(cfg.PiperModel); statErr != nil || !info.Mode().IsRegular() || info.Size() == 0 || !speech.ValidPiperConfig(cfg.PiperModel+".json") {
				return errors.New("configured Piper model is unavailable")
			}
			if info, statErr := os.Stat(cfg.STTModel); statErr != nil || !info.Mode().IsRegular() || info.Size() == 0 {
				return errors.New("configured Whisper model is unavailable")
			}
		}
		return nil
	}}
	// Voice installation and static prompt regeneration can legitimately take
	// several minutes on a fresh volume.  The settings handler has a 30-minute
	// operation context, so a 30-second server write deadline would close the
	// connection while the operation is still succeeding.
	server := &http.Server{Addr: cfg.HTTPAddr, Handler: webApp.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 35 * time.Minute, IdleTimeout: 60 * time.Second}
	appContext, stopApp := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopApp()

	indexer := &mailindex.Indexer{Store: db}
	syncService = &mailsync.Service{Store: db, Root: cfg.DataDir, Runner: mailsync.Runner{}, Index: indexer, Secrets: secrets, Log: log, EndpointChecker: mailsync.CheckPublicIMAPEndpoint}
	bridgeService.Refresher = syncService
	webApp.Sync = syncService

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := syncService.Run(appContext); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("sync service stopped", "error", err)
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := alertService.Run(appContext); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("alert service stopped", "error", err)
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := sipBridge.Apply(appContext); err != nil {
			log.Warn("baresip did not start", "error", err)
		}
		if err := bridgeService.Run(appContext); err != nil && !errors.Is(err, context.Canceled) {
			log.Warn("call bridge stopped", "error", err)
		}
	}()
	serverFailed := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Info("web server listening", "addr", cfg.HTTPAddr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("web server stopped", "error", err)
			serverFailed <- err
			stopApp()
		}
	}()

	<-appContext.Done()
	shutdown, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdown); err != nil {
		log.Error("graceful shutdown failed", "error", err)
	}
	sipBridge.Stop()
	wg.Wait()
	if err := db.Close(); err != nil {
		log.Error("closing database", "error", err)
	}
	var serverErr error
	select {
	case serverErr = <-serverFailed:
	default:
	}
	if serverErr != nil {
		os.Exit(1)
	}
}
