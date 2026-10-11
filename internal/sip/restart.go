package sip

import (
	"context"
	"os/exec"
	"syscall"
	"time"
)

// Failure history belongs to the supervisor rather than an individual spawn
// attempt. Only a child surviving the stable interval resets that history.
type restartPolicy struct {
	initial time.Duration
	max     time.Duration
	stable  time.Duration
}

func (p restartPolicy) policy() restartPolicy {
	if p.initial <= 0 {
		p.initial = initialRespawnBackoff
	}
	if p.max <= 0 {
		p.max = maxRespawnBackoff
	}
	if p.max < p.initial {
		p.max = p.initial
	}
	if p.stable <= 0 {
		p.stable = stableRespawnUptime
	}
	return p
}

func (p restartPolicy) delay(crashes uint8) time.Duration {
	backoff := p.initial
	for step := uint8(1); step < crashes && backoff < p.max; step++ {
		if backoff > p.max/2 {
			return p.max
		}
		backoff *= 2
	}
	return backoff
}

// Requires b.mu. Saturating the counter bounds state even in indefinite
// crash loops; 64 doublings already exceed any representable duration.
func (b *Baresip) recordCrashLocked() {
	if b.crashes < 64 {
		b.crashes++
	}
}

func (b *Baresip) resetAfterStableUptime(cmd *exec.Cmd, waitDone <-chan struct{},
	runCtx context.Context, generation uint64, stable time.Duration) {
	timer := time.NewTimer(stable)
	defer timer.Stop()
	select {
	case <-runCtx.Done():
		return
	case <-waitDone:
		return
	case <-timer.C:
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	// cmd.Wait may still be draining output from an already-exited child.
	// Signal(0) checks actual process lifetime without racing ProcessState.
	if b.cmd == cmd && b.generation == generation && !b.stopping &&
		runCtx.Err() == nil && cmd.Process.Signal(syscall.Signal(0)) == nil {
		b.crashes = 0
	}
}
