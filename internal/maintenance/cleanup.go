// Package maintenance bounds recurring retention of known disposable
// artifacts. Maildirs, models and durable draft generations are never scanned.
package maintenance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	DefaultEphemeralMaxAge = 24 * time.Hour
	// Per directory, including entries that do not match a cleanup rule.
	CleanupEntriesPerDirectory = 128
	// Filesystem nodes visited while removing one abandoned staging tree.
	CleanupTreeNodes = 128
)

type CleanupReport struct {
	RemovedFiles int
	RemovedBytes int64
	Visited      int
}

type cleanupRule struct {
	dir      string
	prefixes []string
	suffixes []string
	dirs     bool
	maxAge   time.Duration
	reader   *os.File
	// Partial tree removal resumes despite directory mtime changing as its
	// children are removed. Identity prevents deleting a replacement tree.
	pending     string
	pendingInfo os.FileInfo
}

// Cleaner retains bounded directory cursors between passes so a large set of
// recent or unrelated entries cannot permanently hide an abandoned artifact.
// It is owned by one maintenance worker and closed before that worker exits.
type Cleaner struct {
	rules  []cleanupRule
	maxAge time.Duration
}

func NewCleaner(root string, maxAge time.Duration) (*Cleaner, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("maintenance root is required")
	}
	if maxAge <= 0 {
		maxAge = DefaultEphemeralMaxAge
	}
	root, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return nil, err
	}
	// Resolve the operator-selected data root once. Symlinks inside its
	// storage classes remain excluded; a deliberate data-root alias works.
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	return &Cleaner{maxAge: maxAge, rules: []cleanupRule{
		{dir: filepath.Join(root, "recordings"), prefixes: []string{".recording-", ".attachment-"}},
		{dir: root, prefixes: []string{".attachment-"}},
		{dir: filepath.Join(root, "prompts"), prefixes: []string{".prompt-", ".greeting-", ".warm-"}},
		{dir: filepath.Join(root, "prompts", "previews"), prefixes: []string{".preview-"}},
		{dir: filepath.Join(root, "prompts", "warm"), prefixes: []string{".warm-"}},
		{dir: filepath.Join(root, "prompts", "dynamic"), prefixes: []string{".dynamic-"}},
		{dir: filepath.Join(root, "prompts", "dynamic"), suffixes: []string{".wav"}, maxAge: 7 * 24 * time.Hour},
		{dir: filepath.Join(root, "run", "speech"), prefixes: []string{".piper-warm-", ".whisper-warm-"}},
		{dir: filepath.Join(root, "config", "mbsync"), prefixes: []string{".conf-"}},
		{dir: filepath.Join(root, "drafts"), prefixes: []string{".draft-stage-"}, dirs: true},
		{dir: filepath.Join(root, "run", "voxmail"), suffixes: []string{".tx.pcm", ".rx.pcm"}},
	}}, nil
}

func (c *Cleaner) Close() error {
	var errs []error
	for i := range c.rules {
		if c.rules[i].reader != nil {
			errs = append(errs, c.rules[i].reader.Close())
			c.rules[i].reader = nil
		}
	}
	return errors.Join(errs...)
}

// CleanupEphemeral is the bounded one-pass startup helper. Runtime workers
// reuse a Cleaner so subsequent passes continue past earlier directory entries.
func CleanupEphemeral(root string, now time.Time, maxAge time.Duration) (CleanupReport, error) {
	return CleanupEphemeralContext(context.Background(), root, now, maxAge)
}

func CleanupEphemeralContext(ctx context.Context, root string, now time.Time, maxAge time.Duration) (CleanupReport, error) {
	c, err := NewCleaner(root, maxAge)
	if err != nil {
		return CleanupReport{}, err
	}
	defer c.Close()
	return c.Clean(context.Background(), now)
}

func (c *Cleaner) Clean(ctx context.Context, now time.Time) (CleanupReport, error) {
	if now.IsZero() {
		now = time.Now()
	}
	var report CleanupReport
	var errs []error
	for i := range c.rules {
		if err := ctx.Err(); err != nil {
			return report, errors.Join(append(errs, err)...)
		}
		age := c.maxAge
		if c.rules[i].maxAge > 0 {
			age = c.rules[i].maxAge
		}
		if err := c.cleanDirectory(ctx, &c.rules[i], now.Add(-age), &report); err != nil {
			errs = append(errs, err)
		}
	}
	return report, errors.Join(errs...)
}

func (c *Cleaner) cleanDirectory(ctx context.Context, rule *cleanupRule, cutoff time.Time, report *CleanupReport) error {
	// Ignore symlinked rule directories and parents, so cleanup cannot enter
	// another storage class through config/, run/ or another path component.
	for path := rule.dir; path != filepath.Dir(path); path = filepath.Dir(path) {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil
		}
	}
	if rule.pending != "" {
		artifacts.Lock()
		info, err := os.Lstat(rule.pending)
		if err == nil && os.SameFile(info, rule.pendingInfo) && !protectedLocked(rule.pending) {
			remaining := CleanupTreeNodes
			complete, removeErr := removeTree(ctx, rule.pending, &remaining, report)
			artifacts.Unlock()
			if complete {
				rule.pending = ""
			}
			return removeErr
		}
		artifacts.Unlock()
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		rule.pending = ""
	}
	current, err := os.Stat(rule.dir)
	if err != nil {
		return err
	}
	if rule.reader != nil {
		opened, err := rule.reader.Stat()
		if err != nil || !os.SameFile(current, opened) {
			_ = rule.reader.Close()
			rule.reader = nil
		}
	}
	if rule.reader == nil {
		rule.reader, err = os.Open(rule.dir)
		if err != nil {
			return err
		}
	}
	names, readErr := rule.reader.Readdirnames(CleanupEntriesPerDirectory)
	if errors.Is(readErr, io.EOF) || readErr == nil && len(names) < CleanupEntriesPerDirectory {
		_ = rule.reader.Close()
		rule.reader = nil
	} else if readErr != nil {
		return readErr
	}
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		report.Visited++
		if !matches(name, rule.prefixes, rule.suffixes) {
			continue
		}
		path := filepath.Join(rule.dir, name)
		artifacts.Lock()
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			artifacts.Unlock()
			continue
		}
		if err != nil {
			artifacts.Unlock()
			return err
		}
		if protectedLocked(path) || info.ModTime().After(cutoff) || info.Mode()&os.ModeSymlink != 0 || info.IsDir() && !rule.dirs {
			artifacts.Unlock()
			continue
		}
		if info.IsDir() {
			remaining := CleanupTreeNodes
			complete, err := removeTree(ctx, path, &remaining, report)
			artifacts.Unlock()
			if !complete {
				rule.pending, rule.pendingInfo = path, info
			}
			// At most one tree budget per directory/pass.
			return err
		}
		if info.Mode().IsRegular() || info.Mode()&os.ModeNamedPipe != 0 {
			err = os.Remove(path)
			if err == nil {
				report.RemovedFiles++
				report.RemovedBytes += info.Size()
			}
		}
		artifacts.Unlock()
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove ephemeral artifact %s: %w", path, err)
		}
	}
	return nil
}

// Called with the artifact lock: no owner can start using this staged tree
// between checking its lease and removing its children. Every filesystem node
// consumes budget; symlinks are unlinked without following their targets.
func removeTree(ctx context.Context, path string, remaining *int, report *CleanupReport) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if *remaining <= 0 {
		return false, nil
	}
	*remaining--
	report.Visited++
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if info.IsDir() {
		dir, err := os.Open(path)
		if err != nil {
			return false, err
		}
		names, err := dir.Readdirnames(*remaining + 1)
		_ = dir.Close()
		if err != nil && !errors.Is(err, io.EOF) {
			return false, err
		}
		for _, name := range names {
			complete, err := removeTree(ctx, filepath.Join(path, name), remaining, report)
			if !complete || err != nil {
				return false, err
			}
		}
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return false, err
	}
	report.RemovedFiles++
	if !info.IsDir() {
		// Directory st_size is filesystem bookkeeping, not bytes the
		// deployment can free, so only files count here.
		report.RemovedBytes += info.Size()
	}
	return true, nil
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
