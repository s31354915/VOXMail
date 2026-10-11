package sip

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// boundedLog retains the current file and one previous file, each at most
// maxBytes. Passing this writer to exec.Cmd routes both output streams through
// Go-owned pipes so rotation replaces the actual write handle during a run.
type boundedLog struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	size     int64
	file     *os.File
}

func newBoundedLog(path string, maxBytes int64) (*boundedLog, error) {
	if maxBytes <= 0 {
		return nil, errors.New("SIP log limit must be positive")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	// Older deployments may already have oversized files. Keep their newest
	// bytes so the total bound holds immediately, even before the first write.
	for _, existing := range []string{path, path + ".1"} {
		if err := capExistingLog(existing, maxBytes); err != nil {
			return nil, err
		}
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return &boundedLog{path: path, maxBytes: maxBytes, size: info.Size(), file: file}, nil
}

func capExistingLog(path string, maxBytes int64) error {
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("SIP log is not a regular file")
	}
	if info.Size() <= maxBytes {
		return os.Chmod(path, 0600)
	}
	staged, err := os.CreateTemp(filepath.Dir(path), ".baresip-log-*")
	if err != nil {
		return err
	}
	defer os.Remove(staged.Name())
	_, copyErr := io.Copy(staged, io.NewSectionReader(file, info.Size()-maxBytes, maxBytes))
	closeErr := staged.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(staged.Name(), path)
}

func (l *boundedLog) Write(data []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return 0, os.ErrClosed
	}
	written := 0
	for len(data) > 0 {
		if l.size == l.maxBytes {
			if err := l.rotate(); err != nil {
				return written, err
			}
		}
		count := len(data)
		if int64(count) > l.maxBytes-l.size {
			count = int(l.maxBytes - l.size)
		}
		n, err := l.file.Write(data[:count])
		l.size += int64(n)
		written += n
		data = data[n:]
		if err != nil {
			return written, err
		}
		if n != count {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}

func (l *boundedLog) rotate() error {
	err := l.file.Close()
	l.file = nil
	if err != nil {
		return err
	}
	if err := os.Rename(l.path, l.path+".1"); err != nil {
		return err
	}
	file, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	l.file, l.size = file, 0
	return nil
}

func (l *boundedLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}
