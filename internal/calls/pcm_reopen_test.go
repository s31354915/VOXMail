package calls

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

type stubPCMWriter struct {
	mu    sync.Mutex
	sizes []int
	errs  []error
}

func (s *stubPCMWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := len(s.sizes)
	s.sizes = append(s.sizes, len(p))
	if index < len(s.errs) {
		return 0, s.errs[index]
	}
	return len(p), nil
}

func (s *stubPCMWriter) Close() error { return nil }

// A broken pipe means the native reader dropped the FIFO: the writer must
// re-open it and resume the same bytes rather than fail the whole prompt.
func TestPacedPCMWriterRecoversFromBrokenPipe(t *testing.T) {
	broken := &stubPCMWriter{errs: []error{unix.EPIPE}}
	healthy := &stubPCMWriter{}
	reopened := make(chan struct{}, pcmReopenAttempts)
	w := &pacedPCMWriter{ctx: context.Background(), w: broken, reopen: func(context.Context) (io.WriteCloser, error) {
		reopened <- struct{}{}
		return healthy, nil
	}}
	payload := make([]byte, 2*(pcmBytesPerSecond/50))
	n, err := w.Write(payload)
	if err != nil {
		t.Fatalf("write err=%v", err)
	}
	if n != len(payload) {
		t.Fatalf("n=%d, want %d", n, len(payload))
	}
	if len(reopened) != 1 {
		t.Fatalf("re-open attempts=%d, want 1", len(reopened))
	}
	if len(broken.sizes) != 1 || broken.sizes[0] != pcmBytesPerSecond/50 {
		t.Fatalf("broken writer sizes=%v", broken.sizes)
	}
	if len(healthy.sizes) != 2 {
		t.Fatalf("healthy writer sizes=%v, want 2 chunks", healthy.sizes)
	}
}

// Without a re-open path the old behavior holds: EPIPE fails the write.
func TestPacedPCMWriterBrokenPipeWithoutReopenFails(t *testing.T) {
	w := &pacedPCMWriter{ctx: context.Background(), w: &stubPCMWriter{errs: []error{unix.EPIPE}}}
	if _, err := w.Write([]byte{1}); !errors.Is(err, unix.EPIPE) {
		t.Fatalf("err=%v, want EPIPE", err)
	}
}

// A reader that keeps dropping the FIFO must not wedge playback: after the
// attempt cap the EPIPE surfaces and the prompt fails as before.
func TestPacedPCMWriterBrokenPipeReopenCap(t *testing.T) {
	reopens := 0
	w := &pacedPCMWriter{ctx: context.Background(), w: &stubPCMWriter{errs: []error{unix.EPIPE}}, reopen: func(context.Context) (io.WriteCloser, error) {
		reopens++
		return &stubPCMWriter{errs: []error{unix.EPIPE}}, nil
	}}
	_, err := w.Write([]byte{1})
	if !errors.Is(err, unix.EPIPE) {
		t.Fatalf("err=%v, want EPIPE", err)
	}
	if reopens != pcmReopenAttempts {
		t.Fatalf("reopens=%d, want %d", reopens, pcmReopenAttempts)
	}
}

// A reader that recovers once and then dies again surfaces the final EPIPE
// after the cap instead of looping forever.
func TestPacedPCMWriterPartialRecoveryThenFailure(t *testing.T) {
	calls := 0
	factory := func() *stubPCMWriter {
		calls++
		if calls == 1 {
			return &stubPCMWriter{errs: []error{unix.EPIPE}}
		}
		return &stubPCMWriter{errs: []error{unix.EPIPE, unix.EPIPE}}
	}
	first := factory()
	w := &pacedPCMWriter{ctx: context.Background(), w: first, reopen: func(context.Context) (io.WriteCloser, error) {
		return factory(), nil
	}}
	_, err := w.Write(make([]byte, pcmBytesPerSecond/50))
	if !errors.Is(err, unix.EPIPE) {
		t.Fatalf("err=%v, want EPIPE", err)
	}
}
