package maintenance

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Artifact creation and cleanup share this lock. Ownership lasts through
// conversion, playback and publication, not merely until a file is closed.
var artifacts = struct {
	sync.Mutex
	active map[string]int
}{active: make(map[string]int)}

func artifactPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	// A publication destination may not exist yet, including its cache
	// directory. Resolve the closest existing ancestor so aliases use the
	// same ownership key before and after creation.
	for parent := abs; ; parent = filepath.Dir(parent) {
		if resolved, err := filepath.EvalSymlinks(parent); err == nil {
			relative, _ := filepath.Rel(parent, abs)
			return filepath.Join(resolved, relative)
		}
		if parent == filepath.Dir(parent) {
			break
		}
	}
	return abs
}

func protectLocked(path string) func() {
	path = artifactPath(path)
	artifacts.active[path]++
	var once sync.Once
	return func() {
		once.Do(func() {
			artifacts.Lock()
			defer artifacts.Unlock()
			artifacts.active[path]--
			if artifacts.active[path] == 0 {
				delete(artifacts.active, path)
			}
		})
	}
}

// Protect keeps a file or directory tree out of maintenance until release.
// Native Baresip owns its entire audio tree while the child is alive, since
// the shim can create paths before delivering their call event to Go.
func Protect(path string) func() {
	if strings.TrimSpace(path) == "" {
		return func() {}
	}
	artifacts.Lock()
	defer artifacts.Unlock()
	return protectLocked(path)
}

func protectedLocked(path string) bool {
	path = artifactPath(path)
	for active := range artifacts.active {
		if path == active || strings.HasPrefix(path, active+string(os.PathSeparator)) || strings.HasPrefix(active, path+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// RemoveIfUnowned lets an artifact's own cache policy share the same lease
// check as periodic cleanup. Active playback is protected from either policy.
func RemoveIfUnowned(path string) (bool, error) {
	artifacts.Lock()
	defer artifacts.Unlock()
	if protectedLocked(path) {
		return false, nil
	}
	err := os.Remove(path)
	return err == nil, err
}

// CreateTemp and MkdirTemp register ownership before cleanup can observe the
// new artifact. Callers defer release immediately and retain their existing
// remove/rename behavior; release is idempotent and never removes user data.
func CreateTemp(dir, pattern string) (*os.File, func(), error) {
	artifacts.Lock()
	defer artifacts.Unlock()
	file, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, nil, err
	}
	return file, protectLocked(file.Name()), nil
}

func MkdirTemp(dir, pattern string) (string, func(), error) {
	artifacts.Lock()
	defer artifacts.Unlock()
	path, err := os.MkdirTemp(dir, pattern)
	if err != nil {
		return "", nil, err
	}
	return path, protectLocked(path), nil
}
