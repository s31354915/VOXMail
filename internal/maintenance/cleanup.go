// Package maintenance contains startup-only cleanup and capacity checks for
// files that are safe to recreate. It deliberately never walks Maildirs,
// model directories, or draft generations recursively: those trees contain
// user data and must be removed only by their owning workflow.
package maintenance

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const DefaultEphemeralMaxAge = 24 * time.Hour

type CleanupReport struct {
	RemovedFiles int
	RemovedBytes int64
}

type cleanupRule struct {
	dir      string
	prefixes []string
	suffixes []string
	dirs     bool
}

// CleanupEphemeral removes only abandoned staging files created by known
// operations. A file must be older than maxAge, and symlinks are ignored so a
// compromised or malformed data directory cannot redirect cleanup elsewhere.
func CleanupEphemeral(root string, now time.Time, maxAge time.Duration) (CleanupReport, error) {
	if strings.TrimSpace(root) == "" {
		return CleanupReport{}, fmt.Errorf("maintenance root is required")
	}
	if maxAge <= 0 {
		maxAge = DefaultEphemeralMaxAge
	}
	if now.IsZero() {
		now = time.Now()
	}
	root, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return CleanupReport{}, err
	}
	rules := []cleanupRule{
		{dir: filepath.Join(root, "recordings"), prefixes: []string{".recording-", ".attachment-"}},
		{dir: filepath.Join(root, "prompts"), prefixes: []string{".prompt-", ".greeting-", ".preview-", ".warm-"}},
		{dir: filepath.Join(root, "run", "speech"), prefixes: []string{".piper-warm-", ".whisper-warm-"}},
		{dir: filepath.Join(root, "config", "mbsync"), prefixes: []string{".conf-"}},
		{dir: filepath.Join(root, "drafts"), prefixes: []string{".draft-stage-"}, dirs: true},
		{dir: filepath.Join(root, "run", "voxmail"), suffixes: []string{".tx.pcm", ".rx.pcm"}},
	}
	var report CleanupReport
	cutoff := now.Add(-maxAge)
	for _, rule := range rules {
		removed, err := cleanupDirectory(rule, cutoff)
		if err != nil {
			return report, err
		}
		report.RemovedFiles += removed.RemovedFiles
		report.RemovedBytes += removed.RemovedBytes
	}
	return report, nil
}

func cleanupDirectory(rule cleanupRule, cutoff time.Time) (CleanupReport, error) {
	entries, err := os.ReadDir(rule.dir)
	if os.IsNotExist(err) {
		return CleanupReport{}, nil
	}
	if err != nil {
		return CleanupReport{}, fmt.Errorf("read ephemeral directory %s: %w", rule.dir, err)
	}
	var report CleanupReport
	for _, entry := range entries {
		name := entry.Name()
		if !matches(name, rule.prefixes, rule.suffixes) {
			continue
		}
		path := filepath.Join(rule.dir, name)
		info, err := os.Lstat(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return report, err
		}
		if info.Mode()&os.ModeSymlink != 0 || info.ModTime().After(cutoff) {
			continue
		}
		if info.IsDir() && !rule.dirs {
			continue
		}
		if !info.IsDir() && !info.Mode().IsRegular() && info.Mode()&os.ModeNamedPipe == 0 {
			continue
		}
		report.RemovedBytes += info.Size()
		if err := os.RemoveAll(path); err != nil {
			return report, fmt.Errorf("remove ephemeral artifact %s: %w", path, err)
		}
		report.RemovedFiles++
	}
	return report, nil
}

func matches(name string, prefixes, suffixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	for _, suffix := range suffixes {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}
