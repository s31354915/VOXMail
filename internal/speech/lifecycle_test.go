package speech

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareStaticPromptsRefreshesWhenModelChanges(t *testing.T) {
	root := t.TempDir()
	model := filepath.Join(root, "voice.onnx")
	if err := os.WriteFile(model, []byte("model-one"), 0600); err != nil {
		t.Fatal(err)
	}
	piper := filepath.Join(root, "fake-piper.sh")
	script := "#!/bin/sh\nout=\nnext=0\nfor arg in \"$@\"; do if [ \"$next\" = 1 ]; then out=$arg; next=0; elif [ \"$arg\" = \"--output_file\" ]; then next=1; fi; done\nprintf 'RIFFstatic' > \"$out\"\n"
	if err := os.WriteFile(piper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	greeting := filepath.Join(root, "prompts", "welcome.wav")
	main := filepath.Join(root, "prompts", "main-menu.wav")
	manifest := filepath.Join(root, "prompts", "static-prompts.json")
	p := Piper{Binary: piper, Model: model}
	first, err := PrepareStaticPrompts(context.Background(), p, greeting, main, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if first.ModelSHA256 == "" {
		t.Fatal("missing model digest")
	}
	if len(first.Assets) != len(StaticPromptTexts()) {
		t.Fatalf("generated %d static assets, want %d", len(first.Assets), len(StaticPromptTexts()))
	}
	for key, relative := range first.Assets {
		if info, err := os.Stat(filepath.Join(filepath.Dir(manifest), relative)); err != nil || info.Size() == 0 {
			t.Fatalf("static asset %q was not activated: %v", key, err)
		}
	}
	if _, err := PrepareStaticPrompts(context.Background(), p, greeting, main, manifest); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(model, []byte("model-two"), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := PrepareStaticPrompts(context.Background(), p, greeting, main, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if first.ModelSHA256 == second.ModelSHA256 {
		t.Fatal("static prompt manifest did not refresh after model change")
	}
}

func TestRuntimePoolEvictsUnusedRuntime(t *testing.T) {
	p := NewRuntimePool("piper", t.TempDir(), "whisper", "model", t.TempDir())
	runtime, lease := p.Activate("voice", 3)
	if runtime == nil || lease == nil {
		t.Fatal("runtime was not activated")
	}
	lease.Release()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.runtimes) != 0 {
		t.Fatalf("runtime pool retained %d unused runtime(s)", len(p.runtimes))
	}
}

func TestPrepareStaticPromptsRejectsManifestPathTraversal(t *testing.T) {
	root := t.TempDir()
	model := filepath.Join(root, "voice.onnx")
	if err := os.WriteFile(model, []byte("model"), 0600); err != nil {
		t.Fatal(err)
	}
	piper := filepath.Join(root, "fake-piper.sh")
	script := "#!/bin/sh\nout=\nnext=0\nfor arg in \"$@\"; do if [ \"$next\" = 1 ]; then out=$arg; next=0; elif [ \"$arg\" = \"--output_file\" ]; then next=1; fi; done\nprintf 'RIFFstatic' > \"$out\"\n"
	if err := os.WriteFile(piper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	prompts := filepath.Join(root, "prompts")
	manifestPath := filepath.Join(prompts, "static-prompts.json")
	if err := os.MkdirAll(prompts, 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside.wav")
	if err := os.WriteFile(outside, []byte("must remain"), 0600); err != nil {
		t.Fatal(err)
	}
	malicious, err := json.Marshal(StaticPromptManifest{Version: 2, VoiceModel: "voice", PromptSHA256: promptDigest(StaticPromptTexts()), WelcomeText: StaticWelcomeText, MainText: StaticMainText, Assets: map[string]string{"welcome": "../outside.wav"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, malicious, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareStaticPrompts(context.Background(), Piper{Binary: piper, Model: model}, filepath.Join(prompts, "welcome.wav"), filepath.Join(prompts, "main.wav"), manifestPath); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "must remain" {
		t.Fatalf("manifest traversal modified outside asset: %q", data)
	}
}
