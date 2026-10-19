package calls

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

const (
	pcmBytesPerSecond = 8000 * 2
	// pcmJitterBuffer is the wall-clock credit the writer keeps ahead of the
	// media clock when pacing begins or after a stall.
	pcmJitterBuffer = 250 * time.Millisecond
	// pcmDrainTail waits out baresip's device pipeline after the FIFO runs
	// dry, so a prompt tail is not clipped by the next prompt or a hangup.
	pcmDrainTail = 500 * time.Millisecond
	// pcmChunkDrainTail is the shorter tail between streamed body chunks:
	// the next chunk is the same speech continuing, so a long tail only
	// adds the pause callers hear between chunks.
	pcmChunkDrainTail = 250 * time.Millisecond
)

// Keep a jitter prebuffer ahead of the media clock, rather than filling
// a multi-second kernel FIFO. The 250 ms credit absorbs scheduler and CPU
// contention stalls (Piper, Whisper, and ffmpeg sharing one box) so the FIFO
// runs dry far less often and speech does not stutter.
type pacedPCMWriter struct {
	ctx  context.Context
	w    io.Writer
	next time.Time
}

func (w *pacedPCMWriter) Write(p []byte) (int, error) {
	sent := 0
	if w.next.IsZero() {
		w.next = time.Now().Add(-pcmJitterBuffer)
	}
	for len(p) > 0 {
		if err := waitPCM(w.ctx, time.Until(w.next)); err != nil {
			return sent, err
		}
		size := min(len(p), pcmBytesPerSecond/50)
		start := time.Now()
		n, err := w.writeResilient(p[:size])
		sent += n
		p = p[n:]
		// Preserve the timeline across normal scheduler latency. Rebase only
		// after a real stall that exhausted the jitter credit, restoring the
		// credit so audio does not gap right after the stall. The threshold
		// must exceed the credit itself, or every paced write would re-fund
		// the credit and defeat the pacing entirely.
		if start.Sub(w.next) > pcmJitterBuffer+100*time.Millisecond {
			w.next = start.Add(-pcmJitterBuffer)
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

// writeResilient retries a chunk that hit EAGAIN. Pollable FIFO writes park
// until the reader drains, but a descriptor that is not pollable can surface
// EAGAIN mid-prompt; treat it as "not writable yet" rather than aborting the
// sentence the caller is in the middle of hearing.
func (w *pacedPCMWriter) writeResilient(p []byte) (int, error) {
	for {
		n, err := w.w.Write(p)
		if err == nil || n > 0 {
			return n, err
		}
		if !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EWOULDBLOCK) {
			return n, err
		}
		if waitErr := waitPCM(w.ctx, 20*time.Millisecond); waitErr != nil {
			return 0, waitErr
		}
	}
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
func drainPCM(ctx context.Context, pipe *os.File, tail time.Duration) error {
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
			// The FIFO is empty, but baresip's decode and device pipeline can
			// still hold more than this; wait out the requested tail so the
			// end of a prompt is not clipped by the next prompt or a hangup.
			return waitPCM(ctx, tail)
		}
		if err := waitPCM(ctx, 10*time.Millisecond); err != nil {
			return err
		}
	}
}
