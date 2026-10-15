package mailsync

import (
	"context"
	"fmt"
	"github.com/voxmail/voxmail/internal/operation"
	"github.com/voxmail/voxmail/internal/process"
	"strings"
	"time"
)

type Runner struct {
	Binary string
	// Timeout optionally caps one mbsync invocation. Zero leaves its lifetime
	// to the caller context, which is important for large initial mailboxes.
	Timeout time.Duration
}

type Result struct {
	Account string
	Output  []byte
	Changed bool
}

const maxMbsyncOutput = process.MaxOutput

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
	cmd := process.New(ctx, "mbsync", r.Binary, r.Timeout, args...)
	output, err := cmd.CombinedOutput()
	result := Result{Account: account, Output: output}
	if err == nil {
		result.Changed = true
		return result, nil
	}
	cause := fmt.Errorf("mbsync %s: %w: %s", account, err, output)
	category := operation.Classify(err)
	// mbsync exposes text rather than a typed protocol API. Interpret known
	// CLI signals once at this adapter; never override cancellation/timeout or
	// expose its output as a user-facing reason.
	if category == operation.ProcessExit {
		lower := strings.ToLower(string(output))
		switch {
		case strings.Contains(lower, "imap tunnel inactivity timeout"):
			category = operation.Timeout
		case strings.Contains(lower, "authentication failed"), strings.Contains(lower, "authenticationfailed"), strings.Contains(lower, "login failed"):
			category = operation.Authentication
		case strings.Contains(lower, "certificate verification failed"), strings.Contains(lower, "ssl error"), strings.Contains(lower, "tls handshake"):
			category = operation.TLS
		}
	}
	return result, &operation.Error{Category: category, Component: "mbsync", Operation: "sync", Cause: cause}
}
