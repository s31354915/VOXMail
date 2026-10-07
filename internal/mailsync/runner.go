package mailsync

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

type Runner struct {
	Binary  string
	Timeout time.Duration
}

type Result struct {
	Account string
	Output  []byte
	Changed bool
}

const maxMbsyncOutput = 1 << 20

type cappedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.limit <= 0 {
		b.limit = maxMbsyncOutput
	}
	remaining := b.limit - b.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			_, _ = b.buf.Write(p[:remaining])
			b.truncated = true
		} else {
			_, _ = b.buf.Write(p)
		}
	} else if len(p) > 0 {
		b.truncated = true
	}
	return len(p), nil
}

func (b *cappedBuffer) Bytes() []byte {
	if !b.truncated {
		return b.buf.Bytes()
	}
	return append(append([]byte(nil), b.buf.Bytes()...), []byte("\n[mbsync output truncated]\n")...)
}

func (r Runner) Sync(ctx context.Context, configPath, channel string) (Result, error) {
	return r.SyncChannels(ctx, configPath, channel)
}

func (r Runner) SyncChannels(ctx context.Context, configPath string, channels ...string) (Result, error) {
	if err := validateChannels(channels); err != nil {
		return Result{}, err
	}
	args := []string{"--config", configPath}
	args = append(args, channels...)
	return r.run(ctx, strings.Join(channels, ","), args...)
}

func validateChannels(channels []string) error {
	if len(channels) == 0 {
		return fmt.Errorf("mbsync channel is required")
	}
	for _, channel := range channels {
		if strings.TrimSpace(channel) == "" {
			return fmt.Errorf("mbsync channel is required")
		}
	}
	return nil
}

func (r Runner) run(ctx context.Context, account string, args ...string) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if r.Binary == "" {
		r.Binary = "mbsync"
	}
	if r.Timeout <= 0 {
		r.Timeout = 10 * time.Minute
	}
	work, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	cmd := exec.CommandContext(work, r.Binary, args...)
	var capture cappedBuffer
	cmd.Stdout = &capture
	cmd.Stderr = &capture
	err := cmd.Run()
	output := capture.Bytes()
	code := 0
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	result := Result{Account: account, Output: output}
	if work.Err() != nil {
		return result, work.Err()
	}
	if err == nil {
		result.Changed = true
		return result, nil
	}
	return result, fmt.Errorf("mbsync %s: %w: %s", account, err, output)
}
