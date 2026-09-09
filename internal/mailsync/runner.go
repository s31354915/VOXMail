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
	code := 0
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	// --ext-exit ORs 32/64 in when the near/far side changed. Those are
	// success indicators, not errors; keep the logic here so callers can
	// trigger indexing only when work actually occurred.
	changed := code&96 != 0
	if work.Err() != nil {
		return Result{Account: channel, Output: output, Changed: changed}, work.Err()
	}
	if err == nil {
		return Result{Account: channel, Output: output}, nil
	}
	if changed {
		return Result{Account: channel, Output: output, Changed: true}, nil
	}
	return Result{Account: channel, Output: output}, fmt.Errorf("mbsync %s: %w: %s", channel, err, output)
}
