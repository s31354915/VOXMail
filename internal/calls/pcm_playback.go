package calls

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

const pcmBytesPerSecond = 8000 * 2

// Keep a small 60 ms prebuffer ahead of the media clock, rather than filling
// a multi-second kernel FIFO. This bounds stale speech after barge-in.
type pacedPCMWriter struct {
	ctx  context.Context
	w    io.Writer
	next time.Time
}

func (w *pacedPCMWriter) Write(p []byte) (int, error) {
	sent := 0
	if w.next.IsZero() {
		w.next = time.Now().Add(-40 * time.Millisecond)
	}
	for len(p) > 0 {
		if err := waitPCM(w.ctx, time.Until(w.next)); err != nil {
			return sent, err
		}
		size := min(len(p), pcmBytesPerSecond/50)
		start := time.Now()
		n, err := w.w.Write(p[:size])
		sent += n
		p = p[n:]
		// Preserve the timeline across normal scheduler latency. Rebase only
		// after a substantial stall so we never send a large catch-up burst.
		if start.Sub(w.next) > 100*time.Millisecond {
			w.next = start
		}
		w.next = w.next.Add(time.Duration(n) * time.Second / pcmBytesPerSecond)
		if err != nil {
			return sent, err
		}
		if n == 0 {
			return sent, io.ErrShortWrite
		}
	}
	return sent, nil
}
func waitPCM(ctx context.Context, duration time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if duration <= 0 {
		return nil
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// FIFO writes acknowledge buffering, not consumption. Wait for the native
// reader before completing playback; allow the final frame through the audio
// buffer before an alert is allowed to hang up.
func drainPCM(ctx context.Context, pipe *os.File) error {
	raw, err := pipe.SyscallConn()
	if err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		pending := 0
		var ioctlErr error
		err := raw.Control(func(fd uintptr) { pending, ioctlErr = unix.IoctlGetInt(int(fd), unix.TIOCINQ) })
		if err != nil {
			return err
		}
		if ioctlErr != nil {
			return fmt.Errorf("PCM drain: %w", ioctlErr)
		}
		if pending == 0 {
			return waitPCM(ctx, 240*time.Millisecond)
		}
		if err := waitPCM(ctx, 10*time.Millisecond); err != nil {
			return err
		}
	}
}
