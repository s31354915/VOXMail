package maintenance

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/voxmail/voxmail/internal/store"
)

const (
	DefaultInterval    = 5 * time.Minute
	DefaultPassTimeout = 5 * time.Second
)

type RetentionStore interface {
	PruneEphemeral(context.Context, time.Time) error
	PruneHistory(context.Context, time.Time, store.HistoryRetentionPolicy) error
}

type maintenanceTicker interface {
	Channel() <-chan time.Time
	Stop()
}

type wallTicker struct{ *time.Ticker }

func (t wallTicker) Channel() <-chan time.Time { return t.C }

// Worker runs serial bounded passes; missed ticks coalesce rather than spawn
// concurrent maintenance jobs. Run returns only after the current pass stops
// and all directory handles close. It performs no network operations.
type Worker struct {
	Store       RetentionStore
	Files       *Cleaner
	Policy      store.HistoryRetentionPolicy
	Log         *slog.Logger
	Interval    time.Duration
	PassTimeout time.Duration
	// Trusted clock seams keep scheduling/expiry tests independent of sleeps.
	now       func() time.Time
	newTicker func(time.Duration) maintenanceTicker
}

func (w *Worker) Run(ctx context.Context) error {
	if w.Files != nil {
		defer w.Files.Close()
	}
	interval := w.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	newTicker := w.newTicker
	if newTicker == nil {
		newTicker = func(d time.Duration) maintenanceTicker { return wallTicker{time.NewTicker(d)} }
	}
	ticker := newTicker(interval)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		w.runPass(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.Channel():
		}
	}
}

func (w *Worker) runPass(parent context.Context) {
	timeout := w.PassTimeout
	if timeout <= 0 {
		timeout = DefaultPassTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	now := time.Now().UTC()
	if w.now != nil {
		now = w.now()
	}
	if w.Store != nil {
		w.report("ephemeral security-row cleanup", w.Store.PruneEphemeral(ctx, now))
		if ctx.Err() == nil {
			w.report("history retention", w.Store.PruneHistory(ctx, now, w.Policy))
		}
	}
	if w.Files != nil && ctx.Err() == nil {
		report, err := w.Files.Clean(ctx, now)
		w.report("ephemeral artifact cleanup", err)
		if w.Log != nil && report.RemovedFiles > 0 {
			w.Log.Info("removed abandoned ephemeral artifacts", "files", report.RemovedFiles, "bytes", report.RemovedBytes)
		}
	}
}

func (w *Worker) report(operation string, err error) {
	if err != nil && !errors.Is(err, context.Canceled) && w.Log != nil {
		w.Log.Warn(operation+" failed", "error", err)
	}
}
