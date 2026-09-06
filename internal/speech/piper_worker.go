package speech

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

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
	if err := writeWithContext(ctx, w.stdin, request); err != nil {
		return err
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if info, err := os.Stat(output); err == nil && info.Size() > 44 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func writeWithContext(ctx context.Context, writer io.Writer, data []byte) error {
	done := make(chan error, 1)
	go func() {
		_, err := writer.Write(data)
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *piperWorker) close() {
	if w == nil {
		return
	}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	_ = w.stdin.Close()
	w.mu.Unlock()
	done := make(chan struct{})
	go func() {
		_ = w.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = w.cmd.Process.Kill()
		<-done
	}
}
