package mailsync

import (
	"context"
	"fmt"
	"os/exec"
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

func (r Runner) Sync(ctx context.Context, configPath, channel string) (Result, error) {
	if r.Binary == "" {
		r.Binary = "mbsync"
	}
	if r.Timeout <= 0 {
		r.Timeout = 10 * time.Minute
	}
	if channel == "" {
		return Result{}, fmt.Errorf("mbsync channel is required")
	}
	work, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	cmd := exec.CommandContext(work, r.Binary, "--config", configPath, "--ext-exit", channel)
	output, err := cmd.CombinedOutput()
	if work.Err() != nil {
		return Result{Account: channel, Output: output}, work.Err()
	}
	if err != nil {
		return Result{Account: channel, Output: output}, fmt.Errorf("mbsync %s: %w: %s", channel, err, output)
	}
	// --ext-exit adds 32/64 when the far/near side changed. Keep this logic
	// here so callers can trigger indexing only when work actually occurred.
	return Result{Account: channel, Output: output, Changed: cmd.ProcessState.ExitCode()&96 != 0}, nil
}
