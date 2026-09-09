package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
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
	staticPromptText := map[string]string{
		"main": speech.StaticMainText,
	}
	mainMenuPath := filepath.Join(promptDir, "main-menu.wav")
	manifestPath := filepath.Join(promptDir, "static-prompts.json")
	promptContext, promptCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	staticManifest, promptErr := speech.PrepareStaticPrompts(promptContext, speech.Piper{Binary: cfg.PiperBinary, Model: cfg.PiperModel}, cfg.GreetingPath, mainMenuPath, manifestPath)
	promptCancel()
	if promptErr != nil {
		log.Warn("static prompts unavailable; only existing recordings will be used", "error", promptErr)
	}
	staticPrompts := make(map[string]string)
	if info, statErr := os.Stat(mainMenuPath); statErr == nil && info.Size() > 44 {
		staticPrompts[staticPromptText["main"]] = mainMenuPath
	}
	db, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Error("cannot open database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

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
	webApp := &web.Server{Store: db, Secrets: secrets, Log: log, SIP: sipBridge}
	server := &http.Server{Addr: cfg.HTTPAddr, Handler: webApp.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	appContext, stopApp := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopApp()
	indexer := &mailindex.Indexer{Store: db}
	syncService := &mailsync.Service{Store: db, Root: cfg.DataDir, Runner: mailsync.Runner{}, Index: indexer, Log: log}
	go func() {
		if err := syncService.Run(appContext); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("sync service stopped", "error", err)
		}
	}()
	go func() {
		speechRuntime := speech.NewRuntime(
			speech.Piper{Binary: cfg.PiperBinary, Model: cfg.PiperModel},
			speech.Whisper{Binary: cfg.STTBinary, Model: cfg.STTModel},
			filepath.Join(cfg.DataDir, "run", "speech"),
		)
		speechPool := speech.NewRuntimePool(
			cfg.PiperBinary, cfg.VoiceDir, cfg.STTBinary, cfg.STTModel,
			filepath.Join(cfg.DataDir, "run", "speech"),
		)
		bridgeService := &calls.Service{
			Socket:   cfg.ControlSocket,
			Store:    db,
			Log:      log,
			MaxCalls: cfg.MaxCalls,
			DataRoot: cfg.DataDir,
			Media: &calls.PromptPlayer{
				Piper:        speech.Piper{Binary: cfg.PiperBinary, Model: cfg.PiperModel},
				Runtime:      speechRuntime,
				Pool:         speechPool,
				Store:        db,
				GreetingPath: cfg.GreetingPath,
				Static:       staticPrompts,
				StaticVoice:  staticManifest.VoiceModel,
				Dir:          promptDir,
			},
			Recorder: &calls.VoiceRecorder{
				Runtime: speechRuntime,
				Whisper: speech.Whisper{Binary: cfg.STTBinary, Model: cfg.STTModel},
				Dir:     cfg.RecordingsDir,
				Window:  15 * time.Second,
			},
			Secrets: secrets,
		}
		alertService := &alerts.Service{Store: db, Bridge: bridgeService, Log: log}
		webApp.Alerts = alertService
		go func() {
			if err := alertService.Run(appContext); err != nil && !errors.Is(err, context.Canceled) {
				log.Error("alert service stopped", "error", err)
			}
		}()
		if err := sipBridge.Apply(appContext); err != nil {
			log.Warn("baresip did not start", "error", err)
		}
		if err := bridgeService.Run(appContext); err != nil && !errors.Is(err, context.Canceled) {
			log.Warn("call bridge stopped", "error", err)
		}
	}()
	go func() {
		log.Info("web server listening", "addr", cfg.HTTPAddr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("web server stopped", "error", err)
			os.Exit(1)
		}
	}()

	<-appContext.Done()
	shutdown, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdown); err != nil {
		log.Error("graceful shutdown failed", "error", err)
	}
	sipBridge.Stop()
}
