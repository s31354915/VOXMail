package speech

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

type Synthesizer interface {
	Synthesize(context.Context, string, string) error
}
type Manifest struct {
	Voice  string            `json:"voice"`
	Speed  int               `json:"speed"`
	Assets map[string]string `json:"assets"`
}

// Cache generates a complete asset set in a staging directory, then atomically
// activates it. A failed generation leaves the previous active cache untouched.
type Cache struct {
	Root  string
	Synth Synthesizer
}

func (c Cache) Paths(voice string, speed int, prompts map[string]string) map[string]string {
	paths := make(map[string]string, len(prompts))
	active := filepath.Join(c.Root, "active")
	for key, text := range prompts {
		digest := sha256.Sum256([]byte(voice + "\x00" + fmt.Sprint(speed) + "\x00" + key + "\x00" + text))
		name := hex.EncodeToString(digest[:]) + ".wav"
		path := filepath.Join(active, name)
		if ValidWAV(path) {
			paths[text] = path
		}
	}
	return paths
}

// maxCacheBuildWorkers bounds concurrent synthesis during cache builds so a
// large prompt set cannot exhaust CPU or process limits.
const maxCacheBuildWorkers = 4

func (c Cache) Build(ctx context.Context, voice string, speed int, prompts map[string]string) (Manifest, error) {
	if c.Root == "" || c.Synth == nil {
		return Manifest{}, fmt.Errorf("cache root and synthesizer are required")
	}
	stage, err := os.MkdirTemp(c.Root, ".stage-")
	if err != nil {
		return Manifest{}, err
	}
	defer os.RemoveAll(stage)
	manifest := Manifest{Voice: voice, Speed: speed, Assets: make(map[string]string, len(prompts))}
	type synthTask struct {
		key, text, name string
	}
	tasks := make([]synthTask, 0, len(prompts))
	for key, text := range prompts {
		digest := sha256.Sum256([]byte(voice + "\x00" + fmt.Sprint(speed) + "\x00" + key + "\x00" + text))
		name := hex.EncodeToString(digest[:]) + ".wav"
		tasks = append(tasks, synthTask{key: key, text: text, name: name})
	}
	// Piper is a per-call process, so bounded parallel synthesis fans a cold
	// warm-up across cores instead of serializing every prompt behind one
	// conversion. Four workers cap concurrent Piper processes; the single
	// prompt case keeps the original serial path.
	workers := runtime.GOMAXPROCS(0)
	if workers > maxCacheBuildWorkers {
		workers = maxCacheBuildWorkers
	}
	if workers > len(tasks) {
		workers = len(tasks)
	}
	if workers < 1 {
		workers = 1
	}
	if workers == 1 {
		for _, task := range tasks {
			if err := c.synthesizeValidated(ctx, task.key, task.text, filepath.Join(stage, task.name)); err != nil {
				return Manifest{}, err
			}
			manifest.Assets[task.key] = task.name
		}
	} else {
		genCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		var (
			wg       sync.WaitGroup
			mu       sync.Mutex
			firstErr error
			jobs     = make(chan synthTask)
		)
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for task := range jobs {
					if err := c.synthesizeValidated(genCtx, task.key, task.text, filepath.Join(stage, task.name)); err != nil {
						mu.Lock()
						if firstErr == nil {
							firstErr = err
						}
						mu.Unlock()
						cancel()
						return
					}
					mu.Lock()
					manifest.Assets[task.key] = task.name
					mu.Unlock()
				}
			}()
		}
	feed:
		for _, task := range tasks {
			select {
			case <-genCtx.Done():
				break feed
			case jobs <- task:
			}
		}
		close(jobs)
		wg.Wait()
		if firstErr != nil {
			return Manifest{}, firstErr
		}
	}
	manifestPath := filepath.Join(stage, "manifest.json")
	file, err := os.Create(manifestPath)
	if err != nil {
		return Manifest{}, err
	}
	if err := json.NewEncoder(file).Encode(manifest); err != nil {
		file.Close()
		return Manifest{}, err
	}
	if err := file.Close(); err != nil {
		return Manifest{}, err
	}
	active := filepath.Join(c.Root, "active")
	old := filepath.Join(c.Root, ".old-active")
	_ = os.RemoveAll(old)
	if err := os.Rename(active, old); err != nil && !os.IsNotExist(err) {
		return Manifest{}, err
	}
	if err := os.Rename(stage, active); err != nil {
		_ = os.Rename(old, active)
		return Manifest{}, err
	}
	_ = os.RemoveAll(old)
	return manifest, nil
}

func (c Cache) synthesizeValidated(ctx context.Context, key, text, path string) error {
	if err := c.Synth.Synthesize(ctx, text, path); err != nil {
		return fmt.Errorf("asset %s: %w", key, err)
	}
	if err := validateWAV(path); err != nil {
		return fmt.Errorf("asset %s produced invalid WAV: %w", key, err)
	}
	return nil
}
