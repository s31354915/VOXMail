package speech

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
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

func TestAtomicInstallPairRollsForwardTogether(t *testing.T) {
	root := t.TempDir()
	stagedModel := filepath.Join(root, "stage-model")
	stagedConfig := filepath.Join(root, "stage-config")
	model := filepath.Join(root, "voice.onnx")
	config := model + ".json"
	for path, data := range map[string]string{stagedModel: "new-model", stagedConfig: `{"audio":{}}`, model: "old-model", config: `{"audio":{"old":true}}`} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := atomicInstallPair(stagedModel, model, stagedConfig, config); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{model: "new-model", config: `{"audio":{}}`} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != want {
			t.Fatalf("%s = %q, want %q", path, data, want)
		}
	}
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
