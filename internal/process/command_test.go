package process

import (
	"context"
	"errors"
	"github.com/voxmail/voxmail/internal/operation"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestExecutionHonorsDeadlineAndDrainsBoundedOutput(t *testing.T) {
	for _, component := range []string{"piper", "whisper", "ffmpeg", "mbsync"} {
		t.Run(component, func(t *testing.T) {
			t.Setenv("VOXMAIL_VERBOSE", "false")
			started := time.Now()
			cmd := New(context.Background(), component, "sh", 80*time.Millisecond, "-c", "while :; do printf 'diagnostic diagnostic diagnostic diagnostic\\n' >&2; done")
			err := cmd.Run()
			if !errors.Is(err, context.DeadlineExceeded) || operation.Classify(err) != operation.Timeout {
				t.Fatalf("error=%v, want deadline", err)
			}
			if time.Since(started) > 2*time.Second {
				t.Fatal("process deadline/join not bounded")
			}
			if len(cmd.StderrText()) > MaxOutput+64 {
				t.Fatal("process output unbounded")
			}
		})
	}
}

func TestContextOwnedCommandHasNoImplicitDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := New(ctx, "mbsync", "sh", 0, "-c", "sleep 30")
	if _, has := cmd.ctx.Deadline(); has {
		t.Fatal("large mailbox received implicit timeout")
	}
	cancel()
	if err := cmd.Run(); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want cancellation", err)
	}
}

func TestCombinedOutputIsBoundedAndPreservesExitStatus(t *testing.T) {
	cmd := New(context.Background(), "fixture", "sh", time.Second, "-c", "printf '%02097152d' 0; exit 7")
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 7 || operation.Classify(err) != operation.ProcessExit {
		t.Fatalf("error=%v, want exit 7", err)
	}
	if len(output) > MaxOutput+64 || !strings.Contains(string(output), "output truncated") {
		t.Fatalf("capture length=%d", len(output))
	}
}
