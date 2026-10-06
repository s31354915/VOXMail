package speech

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDownloadVerifiedRejectsChecksumMismatchAndLeavesNoDestination(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not-the-expected-model"))
	}))
	defer server.Close()
	destination := filepath.Join(t.TempDir(), "model.onnx")
	if err := downloadVerified(context.Background(), server.URL, destination, "0000000000000000000000000000000000000000000000000000000000000000"); err == nil {
		t.Fatal("checksum mismatch was accepted")
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("destination exists after failed verification: %v", err)
	}
}

func TestDownloadVerifiedAcceptsExpectedChecksum(t *testing.T) {
	body := []byte(`{"audio":{"sample_rate":22050}}`)
	digest := sha256.Sum256(body)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer server.Close()
	destination := filepath.Join(t.TempDir(), "voice.json")
	if err := downloadVerified(context.Background(), server.URL, destination, hex.EncodeToString(digest[:])); err != nil {
		t.Fatal(err)
	}
	if !validPiperConfig(destination) {
		t.Fatal("verified sidecar was not recognized as valid Piper JSON")
	}
}

func TestActiveVoiceManifestSelectsAnImmutablePair(t *testing.T) {
	root := t.TempDir()
	name := "local_voice"
	version := "version-one"
	model := filepath.Join(root, voiceVersionsDir, name, version, name+".onnx")
	config := model + ".json"
	if err := os.MkdirAll(filepath.Dir(model), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(model, []byte("model-one"), 0600); err != nil {
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
	if err := writeActiveVoiceManifest(root, activeVoiceManifest{Version: 1, Voice: name, Model: filepath.ToSlash(filepath.Join(voiceVersionsDir, name, version, name+".onnx")), Config: filepath.ToSlash(filepath.Join(voiceVersionsDir, name, version, name+".onnx.json")), ModelSHA256: modelDigest, ConfigSHA256: configDigest}); err != nil {
		t.Fatal(err)
	}
	got, ok := InstalledVoicePath(root, name)
	if !ok || got != model {
		t.Fatalf("active model path=%q,%v want %q,true", got, ok, model)
	}
	if !IsInstalledVoice(root, name) {
		t.Fatal("active usable pair was not recognized")
	}
	if err := os.WriteFile(config, []byte(`{"truncated":`), 0600); err != nil {
		t.Fatal(err)
	}
	if IsInstalledVoice(root, name) {
		t.Fatal("corrupt active sidecar remained usable")
	}
}

func TestActiveVoiceManifestRejectsUnversionedAndSymlinkedPairs(t *testing.T) {
	root := t.TempDir()
	name := "local_voice"
	version := "version-one"
	model := filepath.Join(root, voiceVersionsDir, name, version, name+".onnx")
	config := model + ".json"
	if err := os.MkdirAll(filepath.Dir(model), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(model, []byte("model-one"), 0600); err != nil {
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
	manifest := activeVoiceManifest{Version: 1, Voice: name, ModelSHA256: modelDigest, ConfigSHA256: configDigest}
	manifest.Model = filepath.ToSlash(filepath.Join(voiceVersionsDir, name, version, name+".onnx"))
	manifest.Config = filepath.ToSlash(filepath.Join(voiceVersionsDir, name, version, name+".onnx.json"))
	if err := writeActiveVoiceManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	if _, ok := InstalledVoicePath(root, name); !ok {
		t.Fatal("valid versioned pair was rejected")
	}

	manifest.Model = name + ".onnx"
	manifest.Config = name + ".onnx.json"
	if err := writeActiveVoiceManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	if _, ok := InstalledVoicePath(root, name); ok {
		t.Fatal("unversioned active pair was accepted")
	}

	manifest.Model = filepath.ToSlash(filepath.Join(voiceVersionsDir, name, version, name+".onnx"))
	manifest.Config = filepath.ToSlash(filepath.Join(voiceVersionsDir, name, "different-version", name+".onnx.json"))
	if err := writeActiveVoiceManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	if _, ok := InstalledVoicePath(root, name); ok {
		t.Fatal("model/config from different versions were accepted")
	}

	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0700); err != nil {
		t.Fatal(err)
	}
	versionPath := filepath.Join(root, voiceVersionsDir, name, "linked-version")
	if err := os.Symlink(outside, versionPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	linkedModel := filepath.Join(outside, name+".onnx")
	linkedConfig := linkedModel + ".json"
	if err := os.WriteFile(linkedModel, []byte("model-linked"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(linkedConfig, []byte(`{"audio":{"sample_rate":22050}}`), 0600); err != nil {
		t.Fatal(err)
	}
	linkedModelDigest, err := ModelSHA256(linkedModel)
	if err != nil {
		t.Fatal(err)
	}
	linkedConfigDigest, err := ModelSHA256(linkedConfig)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Model = filepath.ToSlash(filepath.Join(voiceVersionsDir, name, "linked-version", name+".onnx"))
	manifest.Config = filepath.ToSlash(filepath.Join(voiceVersionsDir, name, "linked-version", name+".onnx.json"))
	manifest.ModelSHA256 = linkedModelDigest
	manifest.ConfigSHA256 = linkedConfigDigest
	if err := writeActiveVoiceManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	if _, ok := InstalledVoicePath(root, name); ok {
		t.Fatal("symlinked version directory was accepted")
	}
}

func TestPreserveVoicePathMovesArtifactsToUniqueRecoveryPath(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "staged")
	if err := os.WriteFile(source, []byte("recover me"), 0600); err != nil {
		t.Fatal(err)
	}
	recovery, err := preserveVoicePath(source, root, ".failed-")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatalf("source still exists after preservation: %v", err)
	}
	data, err := os.ReadFile(recovery)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "recover me" {
		t.Fatalf("recovery contents=%q", data)
	}
}

func TestVoiceOperationSerializesAndHonorsCancellation(t *testing.T) {
	root := t.TempDir()
	first, err := AcquireVoiceOperation(context.Background(), root, "voice")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := AcquireVoiceOperation(ctx, root, "voice"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked operation error=%v, want deadline exceeded", err)
	}
	other, err := AcquireVoiceOperation(context.Background(), root, "other")
	if err != nil {
		t.Fatal(err)
	}
	other.Close()
}

func TestInstalledVoiceDiscoveryRequiresUsablePair(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "local_voice.onnx"), []byte("model"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "local_voice.onnx.json"), []byte(`{"audio":{"sample_rate":22050}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "orphan.onnx"), []byte("model"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := InstalledVoiceNames(root); len(got) != 1 || got[0] != "local_voice" {
		t.Fatalf("installed voices=%v, want [local_voice]", got)
	}
	if spec, ok := ResolveVoice(root, "local_voice"); !ok || spec.Voice != "local_voice" || spec.ModelURL != "" {
		t.Fatalf("local voice resolution=%+v, %v", spec, ok)
	}
	if _, ok := ResolveVoice(root, "../local_voice"); ok {
		t.Fatal("path traversal voice was accepted")
	}
}
