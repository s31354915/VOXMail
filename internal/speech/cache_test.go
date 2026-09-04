package speech

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

type fakeSynth struct{}

func (fakeSynth) Synthesize(_ context.Context, _, output string) error {
	return os.WriteFile(output, []byte("wav"), 0600)
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
