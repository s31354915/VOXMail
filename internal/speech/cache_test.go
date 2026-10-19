package speech

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

type fakeSynth struct{}

func (fakeSynth) Synthesize(_ context.Context, _, output string) error {
	return writeTestWAV(output)
}

func TestCacheBuildIsAtomic(t *testing.T) {
	root := t.TempDir()
	c := Cache{Root: root, Synth: fakeSynth{}}
	manifest, err := c.Build(context.Background(), "voice", 3, map[string]string{"welcome": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Assets["welcome"] == "" {
		t.Fatal("asset missing")
	}
	if _, err := os.Stat(filepath.Join(root, "active", "manifest.json")); err != nil {
		t.Fatal(err)
	}
}

// Bounded parallel builds must still produce every asset and stay atomic:
// a failed synthesis aborts the build and leaves the previous cache active.
func TestCacheBuildParallelCompletesAllAssets(t *testing.T) {
	root := t.TempDir()
	c := Cache{Root: root, Synth: fakeSynth{}}
	prompts := map[string]string{}
	for i := 0; i < 12; i++ {
		prompts["prompt-"+string(rune('a'+i))] = "text " + string(rune('a'+i))
	}
	manifest, err := c.Build(context.Background(), "voice", 3, prompts)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Assets) != len(prompts) {
		t.Fatalf("expected %d assets, got %d", len(prompts), len(manifest.Assets))
	}
	for _, name := range manifest.Assets {
		if _, err := os.Stat(filepath.Join(root, "active", name)); err != nil {
			t.Fatalf("asset %s missing: %v", name, err)
		}
	}
}

type failingOnceSynth struct {
	failAfter int
	mu        sync.Mutex
	calls     int
}

func (f *failingOnceSynth) Synthesize(_ context.Context, _, output string) error {
	f.mu.Lock()
	f.calls++
	fail := f.calls > f.failAfter
	f.mu.Unlock()
	if fail {
		return errors.New("synth failed")
	}
	return writeTestWAV(output)
}

func TestCacheBuildParallelFailsAtomically(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "active"), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := filepath.Join(root, "active", "keep.wav")
	if err := writeTestWAV(seed); err != nil {
		t.Fatal(err)
	}
	c := Cache{Root: root, Synth: &failingOnceSynth{failAfter: 2}}
	prompts := map[string]string{}
	for i := 0; i < 10; i++ {
		prompts["p"+string(rune('a'+i))] = "text " + string(rune('a'+i))
	}
	if _, err := c.Build(context.Background(), "voice", 3, prompts); err == nil {
		t.Fatal("expected build error")
	}
	// The staged build must not have replaced the active cache.
	if _, err := os.Stat(filepath.Join(root, "active", "keep.wav")); err != nil {
		t.Fatalf("previous active cache disturbed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "active", "manifest.json")); !os.IsNotExist(err) {
		t.Fatalf("stale manifest should not be active: %v", err)
	}
}
