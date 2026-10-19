package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
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
	"github.com/voxmail/voxmail/internal/diagnostics"
	"github.com/voxmail/voxmail/internal/lifecycle"
	"github.com/voxmail/voxmail/internal/mailindex"
	"github.com/voxmail/voxmail/internal/mailsync"
	"github.com/voxmail/voxmail/internal/maintenance"
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
	operation, err := speech.AcquireVoiceOperation(ctx, v.VoiceDir, voice.Voice)
	if err != nil {
		return err
	}
	defer operation.Close()
	if voice.ModelURL != "" {
		// Trusted catalog models are downloaded only through the pinned,
		// checksum-verified installer. Locally installed models are never
		// replaced or downloaded as a side effect of selecting them.
		if err := operation.InstallVoice(ctx); err != nil {
			return err
		}
	}
	model, ok := speech.InstalledVoicePath(v.VoiceDir, voice.Voice)
	if !ok {
		return errors.New("voice model and sidecar are not installed as a valid pair")
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
				if speech.ValidWAV(path) {
					assets[text] = path
				}
			}
		}
		if len(assets) == 0 {
			return errors.New("voice prompt generation produced no usable assets")
		}
		if v.Player != nil {
			fingerprint, _ := speech.ModelFingerprint(model)
			v.Player.SetStaticPromptsForSpeed(assets, manifest.VoiceModel, speed, fingerprint)
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
	model, ok := speech.InstalledVoicePath(v.VoiceDir, voice.Voice)
	if !ok {
		return nil, errors.New("voice model and sidecar are not installed as a valid pair")
	}
	if speed < 1 || speed > 5 {
		speed = 3
	}
	dir := filepath.Join(v.PromptDir, "previews")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	tmp, releaseTmp, err := maintenance.CreateTemp(dir, ".preview-*.wav")
	if err != nil {
		return nil, err
	}
	defer releaseTmp()
	path := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	defer os.Remove(path)
	if err := (speech.Piper{Binary: v.PiperBinary, Model: model, Extra: speech.SpeedExtra(speed)}).Synthesize(ctx, "This is a VOXMail voice preview.", path); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

// promptWarmState carries the background static-prompt warm-up result.
// The web console reads it under the lock so status answers stay race-free
// while generation runs.
type promptWarmState struct {
	mu       sync.Mutex
	manifest speech.StaticPromptManifest
	err      error
	warming  bool
}

// warmStaticPrompts prepares the static greeting and main-menu prompts in
// the background and publishes the result to the prompt player. A failure
// keeps the player on dynamic synthesis and the previous manifest's assets.
func warmStaticPrompts(ctx context.Context, cfg config.Config, log *slog.Logger, piperModel, promptDir, mainMenuPath, manifestPath string, state *promptWarmState, player *calls.PromptPlayer, fingerprint string) {
	promptContext, promptCancel := context.WithTimeout(ctx, 5*time.Minute)
	defer promptCancel()
	manifest, promptErr := speech.PrepareStaticPrompts(promptContext, speech.Piper{Binary: cfg.PiperBinary, Model: piperModel}, cfg.GreetingPath, mainMenuPath, manifestPath)
	state.mu.Lock()
	state.manifest = manifest
	state.err = promptErr
	state.warming = false
	state.mu.Unlock()
	if promptErr != nil {
		log.Warn("static prompts unavailable; only existing recordings will be used", "error", promptErr)
	}
	staticPrompts := make(map[string]string)
	staticTexts := speech.StaticPromptTexts()
	for key, relative := range manifest.Assets {
		if text, ok := staticTexts[key]; ok {
			path := filepath.Join(promptDir, relative)
			if speech.ValidWAV(path) {
				staticPrompts[text] = path
			}
		}
	}
	// Older deployments have a version-one manifest containing only the main
	// menu. Keep that shipped asset usable until the next successful model
	// regeneration upgrades the manifest.
	if len(staticPrompts) == 0 {
		if speech.ValidWAV(mainMenuPath) {
			staticPrompts[speech.StaticMainText] = mainMenuPath
		}
	}
	if promptErr == nil && player != nil {
		player.SetStaticPromptsForSpeed(staticPrompts, manifest.VoiceModel, 3, fingerprint)
	}
}

func main() {
	const shutdownBudget = 45 * time.Second
	log := slog.New(slog.NewJSONHandler(os.Stdout, diagnostics.HandlerOptions()))
	slog.SetDefault(log)
	// Install signal handling before startup work so model provisioning and
	// prompt generation can be interrupted. The long-lived runtime context is
	// separate: shutdown must keep the SIP bridge alive long enough to hang up
	// active calls before it stops Baresip.
	signalContext, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
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
	fileCleanupCtx, fileCleanupCancel := context.WithTimeout(signalContext, maintenance.DefaultPassTimeout)
	if report, cleanupErr := maintenance.CleanupEphemeralContext(fileCleanupCtx, cfg.DataDir, time.Now().UTC(), maintenance.DefaultEphemeralMaxAge); cleanupErr != nil {
		log.Warn("ephemeral artifact cleanup failed", "error", cleanupErr)
	} else if report.RemovedFiles > 0 {
		log.Info("removed abandoned ephemeral artifacts", "files", report.RemovedFiles, "bytes", report.RemovedBytes)
	}
	fileCleanupCancel()
	if usage, diskErr := maintenance.StatDisk(cfg.DataDir); diskErr != nil {
		log.Warn("could not inspect data filesystem capacity", "error", diskErr)
	} else if usage.FreeBytes < 1<<30 {
		log.Warn("data filesystem is low on free space", "free_bytes", usage.FreeBytes, "total_bytes", usage.TotalBytes)
	}
	if provision, _ := strconv.ParseBool(os.Getenv("VOXMAIL_PROVISION_MODELS")); provision && !cfg.OfflineMode {
		modelContext, cancel := context.WithTimeout(signalContext, 30*time.Minute)
		if err := speech.Provision(modelContext, cfg.VoiceDir, cfg.STTModel); err != nil {
			cancel()
			log.Error("model provisioning failed", "error", err)
			os.Exit(1)
		}
		cancel()
	}
	piperModel := speech.ResolveConfiguredModel(cfg.VoiceDir, cfg.PiperModel)
	promptDir := filepath.Join(cfg.DataDir, "prompts")
	mainMenuPath := filepath.Join(promptDir, "main-menu.wav")
	manifestPath := filepath.Join(promptDir, "static-prompts.json")
	// Static prompt warm-up no longer blocks the listener. The HTTP server
	// binds first, so health checks and the web console answer during a
	// cold start's prompt regeneration instead of holding /readyz for the
	// whole synthesis window (INC-2026-0612, VXE-4). The warm-up job
	// started below fills promptWarm and hands the result to the prompt
	// player; until then calls fall back to on-demand synthesis.
	promptWarm := &promptWarmState{warming: true}
	db, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Error("cannot open database", "error", err)
		os.Exit(1)
	}
	securityCleanupCtx, securityCleanupCancel := context.WithTimeout(signalContext, maintenance.DefaultPassTimeout)
	if interrupted, cleanupErr := db.InterruptSyncRuns(securityCleanupCtx); cleanupErr != nil {
		log.Error("cannot finalize interrupted synchronization runs", "error", cleanupErr)
		securityCleanupCancel()
		_ = db.Close()
		os.Exit(1)
	} else if interrupted > 0 {
		log.Info("finalized interrupted synchronization runs", "runs", interrupted)
	}
	if cleanupErr := db.PruneEphemeral(securityCleanupCtx, time.Now().UTC()); cleanupErr != nil {
		log.Warn("ephemeral security-row cleanup failed", "error", cleanupErr)
	}
	if cleanupErr := db.PruneHistory(securityCleanupCtx, time.Now().UTC(), store.HistoryRetentionPolicy{
		MaxAge:  time.Duration(cfg.HistoryRetentionDays) * 24 * time.Hour,
		MaxRows: cfg.HistoryMaxRows,
	}); cleanupErr != nil {
		log.Warn("durable history retention cleanup failed", "error", cleanupErr)
	}
	securityCleanupCancel()

	sipBridge := &sip.Baresip{
		Binary:        cfg.BaresipBinary,
		ConfigDir:     cfg.BaresipConfig,
		ControlSocket: cfg.ControlSocket,
		AudioDir:      filepath.Join(cfg.DataDir, "run", "voxmail"),
		LogPath:       filepath.Join(cfg.DataDir, "logs", "baresip.log"),
		MaxCalls:      cfg.MaxCalls,
		Offline:       cfg.OfflineMode,
		Store:         db,
		Secrets:       secrets,
		Log:           log,
	}
	speechRuntime := speech.NewRuntime(
		speech.Piper{Binary: cfg.PiperBinary, Model: piperModel},
		speech.Whisper{Binary: cfg.STTBinary, Model: cfg.STTModel},
		filepath.Join(cfg.DataDir, "run", "speech"),
	)
	speechPool := speech.NewRuntimePool(
		cfg.PiperBinary, cfg.VoiceDir, cfg.STTBinary, cfg.STTModel,
		filepath.Join(cfg.DataDir, "run", "speech"),
	)
	promptPlayer := &calls.PromptPlayer{
		Piper:        speech.Piper{Binary: cfg.PiperBinary, Model: piperModel},
		Runtime:      speechRuntime,
		Pool:         speechPool,
		Store:        db,
		GreetingPath: cfg.GreetingPath,
		Dir:          promptDir,
	}
	promptPlayer.PruneDynamicPrompts()
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
			Window:  30 * time.Second,
		},
		Secrets:              secrets,
		IMAPEndpointResolver: mailsync.CheckPublicIMAPEndpoint,
		RegistrationState:    sipBridge.SetRegistrationState,
	}
	alertService := &alerts.Service{Store: db, Bridge: bridgeService, Log: log}
	voiceService := &voiceActivator{VoiceDir: cfg.VoiceDir, PiperBinary: cfg.PiperBinary, PromptDir: promptDir, GreetingPath: cfg.GreetingPath, MainPath: mainMenuPath, ManifestPath: manifestPath, Player: promptPlayer}
	var syncService *mailsync.Service
	runtimeStatus := func(ctx context.Context) web.RuntimeStatus {
		sipStatus := sipBridge.Status()
		bridgeState := "disconnected"
		if bridgeService.Healthy() {
			bridgeState = "connected"
		}
		mediaState := "not_required"
		if sipStatus.Enabled {
			mediaState = "unavailable"
			if info, statErr := os.Stat(piperModel); statErr == nil && info.Mode().IsRegular() && info.Size() > 0 && speech.ValidPiperConfig(piperModel+".json") {
				if info, statErr := os.Stat(cfg.STTModel); statErr == nil && info.Mode().IsRegular() && info.Size() > 0 {
					mediaState = "ready"
				}
			}
		}
		promptState := "ready"
		promptError := ""
		promptWarm.mu.Lock()
		warmManifest, warmErr, warming := promptWarm.manifest, promptWarm.err, promptWarm.warming
		promptWarm.mu.Unlock()
		switch {
		case warmErr != nil:
			promptState = "backup"
			if !speech.ValidWAV(cfg.GreetingPath) {
				promptState = "unavailable"
			}
			promptError = "Configured voice prompts could not be generated; check server logs."
		case warming:
			promptState = "backup"
			promptError = "Voice prompts are regenerating in the background; calls synthesize on demand until they finish."
		}
		return web.RuntimeStatus{PromptState: promptState, PromptVoice: warmManifest.VoiceModel, PromptError: promptError, Process: sipStatus.Process, Bridge: bridgeState, Registration: sipStatus.Registration, Media: mediaState, LastError: sipStatus.LastError, ErrorCategory: sipStatus.ErrorCategory, ResponseCode: sipStatus.ResponseCode}
	}
	voiceInstaller := speech.AdministratorVoiceInstaller{Validate: func(ctx context.Context, model string) error {
		return (speech.Piper{Binary: cfg.PiperBinary, Model: model}).Warm(ctx, filepath.Join(cfg.DataDir, "run", "speech"))
	}}
	webApp := &web.Server{VoiceInstaller: voiceInstaller, Store: db, Secrets: secrets, Log: log, SIP: sipBridge, Alerts: alertService, Voice: voiceService, Preview: voiceService, Calls: bridgeService, DataRoot: cfg.DataDir, VoiceDir: cfg.VoiceDir, OfflineMode: cfg.OfflineMode, TrustedProxyCIDRs: cfg.TrustedProxyCIDRs, RuntimeStatus: runtimeStatus, Ready: func(ctx context.Context) error {
		if cfg.OfflineMode {
			return nil
		}
		if sipBridge.Enabled() {
			sipStatus := sipBridge.Status()
			if sipStatus.Process != sip.ProcessRunning {
				return fmt.Errorf("SIP process is %s", sipStatus.Process)
			}
			if sipStatus.Registration != sip.RegistrationRegistered {
				return fmt.Errorf("SIP registration is %s", sipStatus.Registration)
			}
			if !bridgeService.Healthy() {
				return errors.New("call bridge not connected")
			}
		}
		if syncService == nil || !syncService.Healthy() {
			return errors.New("mail synchronization worker is not running")
		}
		if sipBridge.Enabled() {
			if info, statErr := os.Stat(piperModel); statErr != nil || !info.Mode().IsRegular() || info.Size() == 0 || !speech.ValidPiperConfig(piperModel+".json") {
				return errors.New("configured Piper model is unavailable")
			}
			if info, statErr := os.Stat(cfg.STTModel); statErr != nil || !info.Mode().IsRegular() || info.Size() == 0 {
				return errors.New("configured Whisper model is unavailable")
			}
		}
		return nil
	}}
	// Long sync, installation and preview work runs in tracked background jobs.
	// The HTTP write budget accommodates the remaining bounded provider checks
	// and SMTP submission without tying mailbox size to a connection deadline.
	application := lifecycle.NewApplication(signalContext, cfg.OfflineMode)
	defer application.StopRuntime()
	webApp.JobContext = application.WorkerContext()
	// Shutdown cancels in-flight HTTP operations such as manual synchronization
	// while the call bridge remains available to send hangup commands.
	server := &http.Server{Addr: cfg.HTTPAddr, Handler: webApp.Handler(), BaseContext: func(net.Listener) context.Context { return application.WorkerContext() }, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 90 * time.Second, IdleTimeout: 60 * time.Second}
	indexer := &mailindex.Indexer{Store: db, Log: log}
	syncService = &mailsync.Service{Store: db, Root: cfg.DataDir, QuarantineRoot: filepath.Join(cfg.DataDir, "quarantine"), Runner: mailsync.Runner{}, Index: indexer, Secrets: secrets, Log: log, EndpointResolver: mailsync.CheckPublicIMAPEndpoint}
	if !cfg.OfflineMode {
		bridgeService.Refresher = syncService
		webApp.Sync = syncService
	}
	sipBridge.SetLifetime(application.RuntimeContext())
	application.Start(lifecycle.LocalWorker, func(ctx context.Context) {
		warmStaticPrompts(ctx, cfg, log, piperModel, promptDir, mainMenuPath, manifestPath, promptWarm, promptPlayer, speechRuntime.Fingerprint)
	})
	application.Start(lifecycle.LocalWorker, func(ctx context.Context) {
		if err := webApp.RunAccountCleanup(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("account cleanup stopped", "error", err)
		}
	})
	artifactCleaner, cleanerErr := maintenance.NewCleaner(cfg.DataDir, maintenance.DefaultEphemeralMaxAge)
	if cleanerErr != nil {
		log.Error("cannot initialize periodic maintenance", "error", cleanerErr)
	} else {
		retention := &maintenance.Worker{Store: db, Files: artifactCleaner, Log: log, Policy: store.HistoryRetentionPolicy{
			MaxAge:  time.Duration(cfg.HistoryRetentionDays) * 24 * time.Hour,
			MaxRows: cfg.HistoryMaxRows,
		}}
		application.Start(lifecycle.LocalWorker, func(ctx context.Context) {
			if err := retention.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Warn("periodic maintenance stopped", "error", err)
			}
		})
	}
	application.Start(lifecycle.ProviderWorker, func(ctx context.Context) {
		if err := syncService.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("sync service stopped", "error", err)
		}
	})
	application.Start(lifecycle.ProviderWorker, func(ctx context.Context) {
		if err := alertService.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("alerts service stopped", "error", err)
		}
	})
	application.Start(lifecycle.CallBridgeWorker, func(ctx context.Context) {
		if err := sipBridge.Apply(ctx); err != nil {
			log.Warn("baresip did not start", "error", err)
		}
		if err := bridgeService.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Warn("call bridge stopped", "error", err)
		}
	})
	serverFailed := make(chan error, 1)
	application.Start(lifecycle.LocalWorker, func(context.Context) {
		log.Info("web server listening", "addr", cfg.HTTPAddr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("web server stopped", "error", err)
			serverFailed <- err
			stopSignals()
		}
	})

	<-signalContext.Done()
	// Cancel long-running web jobs before stopping the HTTP listener. The
	// server protects voice-job admission so this cannot race its wait group.
	webApp.BeginShutdown()
	application.BeginShutdown()
	shutdown, shutdownCancel := context.WithTimeout(context.Background(), shutdownBudget)
	defer shutdownCancel()
	if !cfg.OfflineMode {
		callDrain, callDrainCancel := context.WithTimeout(shutdown, 5*time.Second)
		if err := bridgeService.Drain(callDrain); err != nil {
			log.Warn("active calls did not drain before shutdown", "error", err)
		}
		callDrainCancel()
	}
	if err := server.Shutdown(shutdown); err != nil {
		log.Error("graceful shutdown failed", "error", err)
		_ = server.Close()
	}
	// Stop accepting HTTP work before cancelling and awaiting jobs started by
	// handlers. This ordering prevents a voice-install request from racing
	// Server.Close's WaitGroup and ensures its final status update lands before
	// the database is closed.
	webApp.Close()
	application.StopRuntime()
	sipBridge.Stop()
	application.Wait()
	alertService.Wait()
	bridgeService.StopTasks()
	bridgeService.Wait()
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
