package speech

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
)

// piperWorker serializes one-shot Piper invocations for a runtime. The
// installed image pins piper-tts==1.3.0, whose CLI consumes plain text lines;
// keeping one process alive with --json-input would silently fail because that
// option belongs to an older C++ CLI and is not accepted by the pinned Python
// package.
type piperWorker struct {
	p      Piper
	mu     sync.Mutex
	runMu  sync.Mutex
	cancel context.CancelFunc
	closed bool
}

func startPiperWorker(ctx context.Context, p Piper) (*piperWorker, error) {
	if p.Binary == "" {
		p.Binary = "piper"
	}
	if p.Model == "" {
		return nil, fmt.Errorf("piper model is required")
	}
	if _, err := exec.LookPath(p.Binary); err != nil {
		return nil, err
	}
	if info, err := os.Stat(p.Model); err != nil || info.Size() == 0 {
		if err == nil {
			err = fmt.Errorf("piper model is empty")
		}
		return nil, err
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	return &piperWorker{p: p}, nil
}

func (w *piperWorker) synthesize(ctx context.Context, text, output string, speed int) error {
	return w.synthesizeSpeed(ctx, text, output, speed, false)
}

// synthesizeForced applies the caller's speech rate even when the runtime
// baked a length scale into Extra: the requested speed replaces the baked
// one, so an on-call speed change works on every provisioning path.
func (w *piperWorker) synthesizeForced(ctx context.Context, text, output string, speed int) error {
	return w.synthesizeSpeed(ctx, text, output, speed, true)
}

func (w *piperWorker) synthesizeSpeed(ctx context.Context, text, output string, speed int, force bool) error {
	if w == nil {
		return fmt.Errorf("piper worker is nil")
	}
	if text == "" || output == "" {
		return fmt.Errorf("piper text and output are required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := safeOutput(output); err != nil {
		return err
	}
	w.runMu.Lock()
	defer w.runMu.Unlock()

	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return fmt.Errorf("piper worker is closed")
	}
	workerCtx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	w.mu.Unlock()
	defer func() {
		cancel()
		w.mu.Lock()
		w.cancel = nil
		w.mu.Unlock()
	}()
	// A shared engine applies the call's speech rate per request; a runtime
	// that baked Extra flags keeps them and skips the override.
	p := w.p
	if speed >= 1 && speed <= 5 {
		if force {
			// Forced speed: drop any baked length scale and apply the
			// caller's rate instead of competing with a duplicate flag.
			filtered := make([]string, 0, len(p.Extra)+2)
			skipNext := false
			for _, flag := range p.Extra {
				if skipNext {
					skipNext = false
					continue
				}
				if flag == "--length_scale" {
					skipNext = true
					continue
				}
				filtered = append(filtered, flag)
			}
			p.Extra = append(SpeedExtra(speed), filtered...)
		} else if !hasLengthScaleExtra(p.Extra) {
			p.Extra = append(SpeedExtra(speed), p.Extra...)
		}
	}
	return synthesizePiper(workerCtx, p, text, output)
}

// alive reports whether this worker has a usable engine; a one-shot worker
// is alive until it is closed.
func (w *piperWorker) alive() bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return !w.closed
}

func (w *piperWorker) close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		if w.cancel != nil {
			w.cancel()
		}
	}
	w.mu.Unlock()
	return nil
}
