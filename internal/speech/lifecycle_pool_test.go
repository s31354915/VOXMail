package speech

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakePoolVoice writes a tiny fake Piper model and returns its path; the
// shell script the pool runs as the Piper binary answers --output_file with
// a minimal valid WAV header.
func fakePoolVoice(t *testing.T, dir, name, contents string) string {
	t.Helper()
	piper := filepath.Join(dir, "fake-piper.sh")
	script := "#!/bin/sh\nout=\nnext=0\nfor arg in \"$@\"; do if [ \"$next\" = 1 ]; then out=$arg; next=0; elif [ \"$arg\" = \"--output_file\" ]; then next=1; fi; done\nprintf 'RIFF\\046\\000\\000\\000WAVEfmt \\020\\000\\000\\000\\001\\000\\001\\000\\100\\037\\000\\000\\200\\076\\000\\000\\002\\000\\020\\000data\\002\\000\\000\\000\\000\\000' > \"$out\"\n"
	if err := os.WriteFile(piper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(dir, name+".onnx")
	if err := os.WriteFile(model, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(model+".json", []byte(`{"audio":{"sample_rate":22050}}`), 0600); err != nil {
		t.Fatal(err)
	}
	return model
}

func pooledWorkerRefs(p *RuntimePool, model string) (int, bool) {
	digest, _ := ModelFingerprint(model)
	p.mu.Lock()
	defer p.mu.Unlock()
	if entry, ok := p.workers[digest]; ok {
		return entry.refs, true
	}
	return 0, false
}

// Two overlapping calls on the same voice, even at different speeds, share
// one pooled engine, and the engine closes once the last call releases.
func TestPoolSharesWorkerAcrossOverlappingActivations(t *testing.T) {
	root := t.TempDir()
	model := fakePoolVoice(t, root, "shared.onnx", "shared-model")
	pool := NewRuntimePool(filepath.Join(root, "fake-piper.sh"), root, filepath.Join(root, "missing-whisper"), "unused", filepath.Join(root, "runtime"), "python3", false)

	first, firstLease := pool.Activate(model, 3)
	if first == nil || firstLease == nil {
		t.Fatal("first activation failed")
	}
	if _, secondLease := pool.Activate(model, 2); secondLease == nil {
		t.Fatal("second activation failed")
	} else {
		defer secondLease.Release()
	}
	third, thirdLease := pool.Activate(model, 5)
	if third == nil || thirdLease == nil {
		t.Fatal("third activation failed")
	}
	if first == third {
		t.Fatal("different speeds should be different runtimes")
	}

	if refs, ok := pooledWorkerRefs(pool, model); !ok || refs != 3 {
		t.Fatalf("pooled worker refs=%d ok=%v, want 3", refs, ok)
	}

	firstLease.Release()
	thirdLease.Release()
	if refs, ok := pooledWorkerRefs(pool, model); !ok || refs != 1 {
		t.Fatalf("worker survived two releases with refs=%d ok=%v, want refs=1", refs, ok)
	}

	if voices := pool.ResidentVoices(); len(voices) != 0 {
		t.Fatalf("ResidentVoices()=%v with the resident engine disabled", voices)
	}
	if pool.ReleaseKeepWarm(model) {
		t.Fatal("keep-warm release succeeded for a voice that was never pinned")
	}
}

// KeepWarm pins survive call releases; only ReleaseKeepWarm lifts a pin,
// and the engine unloads with the last reference.
func TestKeepWarmPinsSurviveReleases(t *testing.T) {
	root := t.TempDir()
	model := fakePoolVoice(t, root, "pinned.onnx", "pinned-model")
	pool := NewRuntimePool(filepath.Join(root, "fake-piper.sh"), root, filepath.Join(root, "missing-whisper"), "unused", filepath.Join(root, "runtime"), "python3", false)

	if pool.KeepWarm(model, 3) == nil {
		t.Fatal("KeepWarm returned no runtime")
	}
	if _, lease := pool.Activate(model, 3); lease == nil {
		t.Fatal("activation failed")
	} else {
		lease.Release()
	}
	if refs, ok := pooledWorkerRefs(pool, model); !ok || refs != 1 {
		t.Fatalf("pinned voice lost its pooled worker after a call release (refs=%d ok=%v)", refs, ok)
	}

	if !pool.ReleaseKeepWarm(model) {
		t.Fatal("ReleaseKeepWarm did not lift the pin")
	}
	if pool.ReleaseKeepWarm(model) {
		t.Fatal("ReleaseKeepWarm lifted an already-released pin twice")
	}
	if _, ok := pooledWorkerRefs(pool, model); ok {
		t.Fatal("worker stayed pooled after the pin was released")
	}
}

// A resident engine pools one resident worker per voice, shared across
// overlapping activations and closed on the last release. A model the
// resident Python engine cannot load falls back to the one-shot CLI, so
// synthesis still succeeds and the voice never shows as loaded.
func TestResidentWorkerLifecycle(t *testing.T) {
	root := t.TempDir()
	model := fakePoolVoice(t, root, "resident.onnx", "resident-model")
	pool := NewRuntimePool(filepath.Join(root, "fake-piper.sh"), root, filepath.Join(root, "missing-whisper"), "unused", filepath.Join(root, "runtime"), "python3", true)

	first, firstLease := pool.Activate(model, 3)
	if first == nil || firstLease == nil {
		t.Fatal("first activation failed")
	}
	second, secondLease := pool.Activate(model, 4)
	if second == nil || secondLease == nil {
		t.Fatal("second activation failed")
	}

	digest, _ := ModelFingerprint(model)
	pool.mu.Lock()
	entry, ok := pool.workers[digest]
	resident, refs := false, 0
	if ok {
		resident, refs = entry.resident, entry.refs
	}
	pool.mu.Unlock()
	if !ok || !resident || refs != 2 {
		t.Fatalf("pooled worker resident=%v refs=%d ok=%v, want resident with refs 2", resident, refs, ok)
	}

	output := filepath.Join(root, "fallback.wav")
	if err := first.Synthesize(context.Background(), "hello", output); err != nil {
		t.Fatalf("resident fallback synthesis failed: %v", err)
	}
	if !ValidWAV(output) {
		t.Fatal("resident fallback synthesis produced no valid WAV")
	}
	if voices := pool.ResidentVoices(); len(voices) != 0 {
		t.Fatalf("ResidentVoices()=%v with an unloadable model, want none", voices)
	}

	firstLease.Release()
	pool.mu.Lock()
	_, still := pool.workers[digest]
	pool.mu.Unlock()
	if !still {
		t.Fatal("resident worker lost from the pool while the second call still holds it")
	}
	secondLease.Release()
	pool.mu.Lock()
	_, ok = pool.workers[digest]
	pool.mu.Unlock()
	if ok {
		t.Fatal("resident worker stayed pooled after the last release")
	}
}

// The pool warms Whisper once, shared by every runtime regardless of voice
// or speed: the STT model is independent of both. A runtime's teardown must
// not disturb the shared state another runtime is waiting on, and a failed
// warm-up must not be cached for the pool's lifetime.
func TestRuntimePoolSharesWhisperWarmAcrossSpeeds(t *testing.T) {
	root := t.TempDir()
	model := fakePoolVoice(t, root, "shared-stt.onnx", "shared-model")
	whisper := filepath.Join(root, "fake-whisper.sh")
	// Warm-up tolerates an empty transcription, so an exit-0 script that
	// prints nothing is a successfully warmed engine.
	if err := os.WriteFile(whisper, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	whisperModel := filepath.Join(root, "whisper-model.bin")
	if err := os.WriteFile(whisperModel, []byte("whisper-model-bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	pool := NewRuntimePool(filepath.Join(root, "fake-piper.sh"), root, whisper, whisperModel, filepath.Join(root, "runtime"), "python3", false)

	first, firstLease := pool.Activate(model, 3)
	if first == nil || firstLease == nil {
		t.Fatal("first activation failed")
	}
	second, secondLease := pool.Activate(model, 4)
	if second == nil || secondLease == nil {
		t.Fatal("second activation failed")
	}
	defer secondLease.Release()
	if first == second {
		t.Fatal("different speeds should build distinct runtimes")
	}
	if first.sttShared == nil || first.sttShared != second.sttShared {
		t.Fatal("pool runtimes must share one Whisper warm state")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := first.WaitWhisper(ctx); err != nil {
		t.Fatalf("first runtime whisper warm: %v", err)
	}
	if err := second.WaitWhisper(ctx); err != nil {
		t.Fatalf("second runtime whisper warm: %v", err)
	}
	// Tearing one runtime down must leave the shared warm state usable.
	firstLease.Release()
	if err := second.WaitWhisper(ctx); err != nil {
		t.Fatalf("shared whisper warm state damaged by peer teardown: %v", err)
	}
	if first.sttShared != second.sttShared || first.sttShared.closed == false {
		t.Fatal("shared warm state should stay open after peer teardown")
	}

	// Standalone runtimes keep their own warm state.
	standalone := NewRuntime(Piper{Binary: filepath.Join(root, "fake-piper.sh"), Model: model}, Whisper{}, filepath.Join(root, "standalone"))
	if standalone.sttShared != nil {
		t.Fatal("standalone runtime must not inherit pool whisper state")
	}
}
