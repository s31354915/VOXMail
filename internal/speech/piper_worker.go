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

func (w *piperWorker) synthesize(ctx context.Context, text, output string) error {
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
	return synthesizePiper(workerCtx, w.p, text, output)
}

func (w *piperWorker) close() {
	if w == nil {
		return
	}
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		if w.cancel != nil {
			w.cancel()
		}
	}
	w.mu.Unlock()
}
