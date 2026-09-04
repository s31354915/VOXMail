package speech

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	for key, text := range prompts {
		digest := sha256.Sum256([]byte(voice + fmt.Sprint(speed) + key + text))
		name := hex.EncodeToString(digest[:]) + ".wav"
		path := filepath.Join(stage, name)
		if err := c.Synth.Synthesize(ctx, text, path); err != nil {
			return Manifest{}, fmt.Errorf("asset %s: %w", key, err)
		}
		manifest.Assets[key] = name
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
