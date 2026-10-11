package process

import (
	"bytes"
	"sync"
)

// Buffer drains all writes while retaining only a bounded prefix. Returning
// the original write length keeps a noisy child from stalling on its pipe.
type Buffer struct {
	mu        sync.Mutex
	data      bytes.Buffer
	limit     int
	truncated bool
}

func NewBuffer(limit int) *Buffer {
	if limit <= 0 {
		limit = MaxOutput
	}
	return &Buffer{limit: limit}
}
func (b *Buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := b.limit - b.data.Len()
	if remaining > 0 {
		count := len(p)
		if count > remaining {
			count = remaining
		}
		_, _ = b.data.Write(p[:count])
	}
	if len(p) > remaining {
		b.truncated = true
	}
	return len(p), nil
}
func (b *Buffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	data := append([]byte(nil), b.data.Bytes()...)
	if b.truncated {
		data = append(data, []byte("\n[output truncated]\n")...)
	}
	return data
}
func (b *Buffer) String() string  { return string(b.Bytes()) }
func (b *Buffer) Truncated() bool { b.mu.Lock(); defer b.mu.Unlock(); return b.truncated }
