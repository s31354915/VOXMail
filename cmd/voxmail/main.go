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

	"github.com/voxmail/voxmail/internal/calls"
	"github.com/voxmail/voxmail/internal/config"
	"github.com/voxmail/voxmail/internal/mailindex"
	"github.com/voxmail/voxmail/internal/mailsync"
	"github.com/voxmail/voxmail/internal/secret"
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
	for _, dir := range []string{cfg.DataDir, filepath.Dir(cfg.DBPath), filepath.Dir(cfg.ControlSocket), filepath.Join(cfg.DataDir, "run", "voxmail"), filepath.Join(cfg.DataDir, "logs"), cfg.VoiceDir, cfg.RecordingsDir} {
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
	db, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Error("cannot open database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	server := &http.Server{Addr: cfg.HTTPAddr, Handler: (&web.Server{Store: db, Secrets: secrets, Log: log}).Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
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
		if os.Getenv("VOXMAIL_SIP_ACCOUNT") == "" && os.Getenv("VOXMAIL_ENABLE_CALLS") != "1" {
			return
		}
		bridgeService := &calls.Service{
			Socket:   cfg.ControlSocket,
			Store:    db,
			Log:      log,
			MaxCalls: cfg.MaxCalls,
			Media: &calls.PromptPlayer{
				Piper: speech.Piper{Binary: cfg.PiperBinary, Model: cfg.PiperModel},
				Dir:   filepath.Join(cfg.DataDir, "prompts"),
			},
			Secrets: secrets,
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
}
