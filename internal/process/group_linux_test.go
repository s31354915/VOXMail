//go:build linux

package process

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCancellationAndEarlyExitStopDescendantsHoldingPipes(t *testing.T) {
	for _, mode := range []string{"deadline", "early-exit"} {
		t.Run(mode, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "child.pid")
			parentAction := "wait"
			limit := 100 * time.Millisecond
			if mode == "early-exit" {
				parentAction = "exit 0"
				limit = 2 * time.Second
			}
			// The grandchild inherits stdout/stderr, reproducing the orphan pipe
			// hang that direct-child cancellation alone fails to bound.
			cmd := New(context.Background(), "fixture", "sh", limit, "-c", `sleep 30 & echo $! > "$1"; `+parentAction, "fixture", pidFile)
			started := time.Now()
			err := cmd.Run()
			if mode == "deadline" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("error=%v", err)
			}
			if mode == "early-exit" && !errors.Is(err, exec.ErrWaitDelay) {
				t.Fatalf("error=%v, want bounded inherited-pipe wait", err)
			}
			if time.Since(started) > 2*time.Second {
				t.Fatal("descendant kept pipe join alive")
			}
			data, readErr := os.ReadFile(pidFile)
			if readErr != nil {
				t.Fatal(readErr)
			}
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			deadline := time.Now().Add(time.Second)
			for {
				stat, readErr := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
				if os.IsNotExist(readErr) {
					break
				}
				// A reparented zombie no longer executes or owns a pipe; host init
				// decides when to reap it.
				end := strings.LastIndex(string(stat), ")")
				if readErr == nil && end >= 0 && strings.HasPrefix(string(stat)[end+1:], " Z") {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("descendant %d still executing", pid)
				}
				time.Sleep(5 * time.Millisecond)
			}
		})
	}
}
