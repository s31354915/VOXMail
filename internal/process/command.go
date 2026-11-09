// Package process owns bounded, cancellable execution of media/sync children.
package process

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"time"

	"github.com/voxmail/voxmail/internal/diagnostics"
	"github.com/voxmail/voxmail/internal/operation"
)

const (
	MaxOutput      = 1 << 20
	PiperTimeout   = 2 * time.Minute
	WhisperTimeout = 5 * time.Minute
	// 30 s was too tight under CPU contention: Piper, Whisper, and the
	// converters share one box, and dictation conversions that lost the
	// race were reported to callers as failures. Two minutes matches the
	// synthesis budget.
	ConversionTimeout = 2 * time.Minute
	PlaybackTimeout   = 10 * time.Minute
	PipeWait          = 500 * time.Millisecond
)

type Command struct {
	*exec.Cmd
	ctx       context.Context
	cancel    context.CancelFunc
	component string
	capture   *Buffer
}

// New uses a caller-owned context and an optional per-operation deadline.
// Zero timeout is intentional for initial mbsync imports: shutdown/caller
// cancellation owns that lifetime, rather than an arbitrary mailbox-size cap.
func New(ctx context.Context, component, binary string, timeout time.Duration, args ...string) *Command {
	if ctx == nil {
		ctx = context.Background()
	}
	work, cancel := context.WithCancel(ctx)
	if timeout > 0 {
		cancel()
		work, cancel = context.WithTimeout(ctx, timeout)
	}
	cmd := exec.CommandContext(work, binary, args...)
	configure(cmd)
	capture := NewBuffer(MaxOutput)
	cmd.Stdout = io.Discard
	cmd.Stderr = capture
	return &Command{Cmd: cmd, ctx: work, cancel: cancel, component: component, capture: capture}
}

func (c *Command) Run() error {
	defer c.cancel()
	defer terminate(c.Cmd)
	err := diagnostics.Run(c.ctx, c.component, c.Cmd)
	if c.ctx.Err() != nil {
		return operation.Wrap(c.component, "execute", operation.Failed, errors.Join(err, c.ctx.Err()))
	}
	return operation.Wrap(c.component, "execute", operation.Failed, err)
}

func (c *Command) CombinedOutput() ([]byte, error) {
	c.Stdout = c.capture
	c.Stderr = c.capture
	err := c.Run()
	return c.capture.Bytes(), err
}

func (c *Command) StderrText() string { return c.capture.String() }

// Configure prepares a long-lived child command the same way New does: its
// own process group so shutdown can kill the whole tree, and the standard
// wait delay. Callers that manage Start/Wait themselves use this instead of
// New.
func Configure(cmd *exec.Cmd) { configure(cmd) }

// Terminate kills a command's whole process group. Safe on an exited or
// never-started command.
func Terminate(cmd *exec.Cmd) { terminate(cmd) }
