package speech

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// piperStallTimeout is how long synthesize waits for the output file to appear
// or grow before declaring the worker stalled and recycling it.
const piperStallTimeout = 2 * time.Minute

type piperWorker struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	mu     sync.Mutex
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
	args := []string{"--model", p.Model, "--json-input"}
	args = append(args, p.Extra...)
	cmd := exec.CommandContext(ctx, p.Binary, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		stdin.Close()
		return nil, err
	}
	return &piperWorker{cmd: cmd, stdin: stdin}, nil
}

func (w *piperWorker) synthesize(ctx context.Context, text, output string) error {
	if text == "" || output == "" {
		return fmt.Errorf("piper text and output are required")
	}
	if err := os.MkdirAll(filepath.Dir(output), 0700); err != nil {
		return err
	}
	request, err := json.Marshal(map[string]string{"text": text, "output_file": output})
	if err != nil {
		return err
	}
	request = append(request, '\n')
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return fmt.Errorf("piper worker is closed")
	}
	if err := w.writeWithContext(ctx, request); err != nil {
		return err
	}
	lastSize := int64(-1)
	lastProgress := time.Now()
	if info, err := os.Stat(output); err == nil {
		lastSize = info.Size()
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		info, err := os.Stat(output)
		if err == nil && info.Size() > 44 {
			return nil
		}
		size := int64(0)
		if err == nil {
			size = info.Size()
		}
		if size != lastSize {
			lastProgress = time.Now()
			lastSize = size
			continue
		}
		if time.Since(lastProgress) > piperStallTimeout {
			w.killLocked()
			return errors.New("piper worker stalled")
		}
		if !w.aliveLocked() {
			return errors.New("piper worker exited")
		}
	}
}

// writeWithContext writes to the worker's stdin without leaking a goroutine:
// if the request is cancelled while the pipe is full, the pending write is
// released by closing the pipe and the worker is marked unusable.
func (w *piperWorker) writeWithContext(ctx context.Context, data []byte) error {
	written := make(chan error, 1)
	go func() {
		_, err := w.stdin.Write(data)
		written <- err
	}()
	select {
	case err := <-written:
		return err
	case <-ctx.Done():
		w.killLocked()
		<-written
		return ctx.Err()
	}
}

func (w *piperWorker) aliveLocked() bool {
	if w.cmd == nil || w.cmd.Process == nil {
		return false
	}
	err := w.cmd.Process.Signal(syscall.Signal(0))
	return err == nil
}

func (w *piperWorker) killLocked() {
	if w.closed {
		return
	}
	w.closed = true
	if w.stdin != nil {
		_ = w.stdin.Close()
	}
	if w.cmd != nil && w.cmd.Process != nil {
		_ = w.cmd.Process.Kill()
	}
}

func (w *piperWorker) close() {
	if w == nil {
		return
	}
	w.mu.Lock()
	alreadyClosed := w.closed
	if !alreadyClosed {
		w.closed = true
		if w.stdin != nil {
			_ = w.stdin.Close()
		}
	}
	w.mu.Unlock()
	if w.cmd == nil {
		return
	}
	if !alreadyClosed && w.cmd.Process != nil {
		_ = w.cmd.Process.Kill()
	}
	done := make(chan struct{})
	go func() {
		_ = w.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
}
