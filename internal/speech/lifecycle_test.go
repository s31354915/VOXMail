package speech

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPrepareStaticPromptsRefreshesWhenModelChanges(t *testing.T) {
	root := t.TempDir()
	model := filepath.Join(root, "voice.onnx")
	if err := os.WriteFile(model, []byte("model-one"), 0600); err != nil {
		t.Fatal(err)
	}
	piper := filepath.Join(root, "fake-piper.sh")
	script := "#!/bin/sh\nout=\nnext=0\nfor arg in \"$@\"; do if [ \"$next\" = 1 ]; then out=$arg; next=0; elif [ \"$arg\" = \"--output_file\" ]; then next=1; fi; done\nprintf 'RIFF\\046\\000\\000\\000WAVEfmt \\020\\000\\000\\000\\001\\000\\001\\000\\100\\037\\000\\000\\200\\076\\000\\000\\002\\000\\020\\000data\\002\\000\\000\\000\\000\\000' > \"$out\"\n"
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
		if !ValidWAV(filepath.Join(filepath.Dir(manifest), relative)) {
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

func TestRuntimePoolUsesActiveModelDigestInRuntimeIdentity(t *testing.T) {
	voiceDir := t.TempDir()
	name := "local_voice"
	writePair := func(version, modelText string) string {
		model := filepath.Join(voiceDir, voiceVersionsDir, name, version, name+".onnx")
		config := model + ".json"
		if err := os.MkdirAll(filepath.Dir(model), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(model, []byte(modelText), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(config, []byte(`{"audio":{"sample_rate":22050}}`), 0600); err != nil {
			t.Fatal(err)
		}
		modelDigest, err := ModelSHA256(model)
		if err != nil {
			t.Fatal(err)
		}
		configDigest, err := ModelSHA256(config)
		if err != nil {
			t.Fatal(err)
		}
		if err := writeActiveVoiceManifest(voiceDir, activeVoiceManifest{
			Version: 1, Voice: name,
			Model:       filepath.ToSlash(filepath.Join(voiceVersionsDir, name, version, name+".onnx")),
			Config:      filepath.ToSlash(filepath.Join(voiceVersionsDir, name, version, name+".onnx.json")),
			ModelSHA256: modelDigest, ConfigSHA256: configDigest,
		}); err != nil {
			t.Fatal(err)
		}
		return model
	}

	firstModel := writePair("one", "model-one")
	p := NewRuntimePool("missing-piper-for-identity-test", voiceDir, "missing-whisper", "missing-model", t.TempDir())
	first, firstLease := p.Activate(name, 3)
	if first == nil || firstLease == nil {
		t.Fatal("first runtime was not activated")
	}
	if first.Piper.Model != firstModel {
		t.Fatalf("first runtime model=%q want %q", first.Piper.Model, firstModel)
	}
	firstLease.Release()

	secondModel := writePair("two", "model-two")
	second, secondLease := p.Activate(name, 3)
	if second == nil || secondLease == nil {
		t.Fatal("second runtime was not activated")
	}
	secondLease.Release()
	if second.Piper.Model != secondModel {
		t.Fatalf("second runtime model=%q want %q", second.Piper.Model, secondModel)
	}
	if first == second {
		t.Fatal("model digest change reused the previous runtime")
	}
}

func TestRuntimeKeepsTTSAvailableWhenWhisperWarmupFails(t *testing.T) {
	root := t.TempDir()
	model := filepath.Join(root, "voice.onnx")
	whisperModel := filepath.Join(root, "whisper.bin")
	for _, path := range []string{model, whisperModel} {
		if err := os.WriteFile(path, []byte("model"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	piper := writeFakePiper(t, partialWAVBody+"\nprintf '\\000\\000' >> \"$out\"")
	whisper := filepath.Join(root, "failing-whisper.sh")
	if err := os.WriteFile(whisper, []byte("#!/bin/sh\necho whisper failed >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	runtime := NewRuntime(Piper{Binary: piper, Model: model}, Whisper{Binary: whisper, Model: whisperModel}, filepath.Join(root, "runtime"))
	lease := runtime.Activate()
	defer lease.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := runtime.WaitTTS(ctx); err != nil {
		t.Fatalf("TTS was blocked by Whisper warmup failure: %v", err)
	}
	if err := runtime.WaitWhisper(ctx); err == nil {
		t.Fatal("Whisper warmup failure was not reported")
	}
	output := filepath.Join(root, "tts.wav")
	if err := runtime.Synthesize(ctx, "hello", output); err != nil {
		t.Fatalf("TTS failed despite independent readiness: %v", err)
	}
	if !ValidWAV(output) {
		t.Fatal("TTS output is not a valid WAV")
	}
}

func TestRuntimeReleaseUnblocksCapabilityWaiters(t *testing.T) {
	root := t.TempDir()
	model := filepath.Join(root, "voice.onnx")
	whisperModel := filepath.Join(root, "whisper.bin")
	for _, path := range []string{model, whisperModel} {
		if err := os.WriteFile(path, []byte("model"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	piper := writeFakePiper(t, partialWAVBody+"\nsleep 5")
	whisper := filepath.Join(root, "slow-whisper.sh")
	if err := os.WriteFile(whisper, []byte("#!/bin/sh\nsleep 5\nprintf ready\n"), 0700); err != nil {
		t.Fatal(err)
	}
	runtime := NewRuntime(Piper{Binary: piper, Model: model}, Whisper{Binary: whisper, Model: whisperModel}, filepath.Join(root, "runtime"))
	lease := runtime.Activate()
	result := make(chan error, 1)
	go func() { result <- runtime.WaitTTS(context.Background()) }()
	time.Sleep(80 * time.Millisecond)
	lease.Release()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("released runtime reported successful readiness")
		}
	case <-time.After(time.Second):
		t.Fatal("runtime release stranded a readiness waiter")
	}
}

func TestPrepareStaticPromptsRejectsManifestPathTraversal(t *testing.T) {
	root := t.TempDir()
	model := filepath.Join(root, "voice.onnx")
	if err := os.WriteFile(model, []byte("model"), 0600); err != nil {
		t.Fatal(err)
	}
	piper := filepath.Join(root, "fake-piper.sh")
	script := "#!/bin/sh\nout=\nnext=0\nfor arg in \"$@\"; do if [ \"$next\" = 1 ]; then out=$arg; next=0; elif [ \"$arg\" = \"--output_file\" ]; then next=1; fi; done\nprintf 'RIFF\\046\\000\\000\\000WAVEfmt \\020\\000\\000\\000\\001\\000\\001\\000\\100\\037\\000\\000\\200\\076\\000\\000\\002\\000\\020\\000data\\002\\000\\000\\000\\000\\000' > \"$out\"\n"
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
	malicious, err := json.Marshal(StaticPromptManifest{Version: staticPromptManifestVersion, VoiceModel: "voice", PromptSHA256: promptDigest(StaticPromptTexts()), WelcomeText: StaticWelcomeText, MainText: StaticMainText, Assets: map[string]string{"welcome": "../outside.wav"}})
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
