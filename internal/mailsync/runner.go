package mailsync

import (
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

func (r Runner) Sync(ctx context.Context, configPath, channel string) (Result, error) {
	return r.SyncChannels(ctx, configPath, channel)
}

func (r Runner) SyncChannels(ctx context.Context, configPath string, channels ...string) (Result, error) {
	if err := validateChannels(channels); err != nil {
		return Result{}, err
	}
	args := []string{"--config", configPath, "--ext-exit"}
	args = append(args, channels...)
	return r.run(ctx, strings.Join(channels, ","), args...)
}

// Validate performs mbsync's parser and endpoint-policy dry run without
// transferring mail. It is deliberately a separate invocation so a malformed
// generated configuration never starts a partial synchronization.
func (r Runner) Validate(ctx context.Context, configPath string, channels ...string) error {
	if r.Binary == "" {
		r.Binary = "mbsync"
	}
	if r.Timeout <= 0 {
		r.Timeout = 10 * time.Minute
	}
	if err := validateChannels(channels); err != nil {
		return err
	}
	args := []string{"--dry-run", "--config", configPath, "--ext-exit"}
	args = append(args, channels...)
	_, err := r.run(ctx, strings.Join(channels, ","), args...)
	return err
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
	if r.Binary == "" {
		r.Binary = "mbsync"
	}
	if r.Timeout <= 0 {
		r.Timeout = 10 * time.Minute
	}
	work, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	cmd := exec.CommandContext(work, r.Binary, args...)
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
		return Result{Account: account, Output: output, Changed: changed}, work.Err()
	}
	if err == nil {
		return Result{Account: account, Output: output}, nil
	}
	if changed {
		return Result{Account: account, Output: output, Changed: true}, nil
	}
	return Result{Account: account, Output: output}, fmt.Errorf("mbsync %s: %w: %s", account, err, output)
}
