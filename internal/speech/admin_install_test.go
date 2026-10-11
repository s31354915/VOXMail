package speech

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAdministratorVoiceResolutionSupportsOtherNamesAndCustomSources(t *testing.T) {
	named, err := ResolveAdministratorVoice(AdministratorVoiceRequest{Voice: "en_US-lessac-low"})
	if err != nil {
		t.Fatal(err)
	}
	if _, trusted := FindTrustedVoice(named.Voice); trusted {
		t.Fatal("fixture must be outside the bundled catalog")
	}
	if named.ModelURL != "https://huggingface.co/rhasspy/piper-voices/resolve/main/en/en_US/lessac/low/en_US-lessac-low.onnx?download=true" || named.ConfigURL != strings.Replace(named.ModelURL, ".onnx?", ".onnx.json?", 1) {
		t.Fatalf("named source resolution: %+v", named)
	}
	custom, err := ResolveAdministratorVoice(AdministratorVoiceRequest{Voice: "https://models.example/voice.onnx?download=true"})
	if err != nil || custom.Voice != "voice" || custom.ConfigURL != "https://models.example/voice.onnx.json?download=true" {
		t.Fatalf("custom source: %+v %v", custom, err)
	}
	trusted, err := ResolveAdministratorVoice(AdministratorVoiceRequest{Voice: BundledPromptVoice})
	if err != nil || trusted.ModelURL != PiperModelURL || trusted.ModelSHA256 != PiperModelSHA256 || trusted.ConfigSHA256 != PiperConfigSHA256 {
		t.Fatal("bundled name lost its pinned identity", trusted, err)
	}
	for _, request := range []AdministratorVoiceRequest{
		{Voice: "../escape", ModelURL: "https://models.example/voice.onnx"},
		{Voice: "https://models.example/.onnx"},
		{Voice: "http://models.example/voice.onnx"},
		{Voice: "file:///private/voice.onnx"},
		{Voice: "https://user:password@models.example/voice.onnx"},
		{Voice: "https://models.example/voice.onnx#fragment"},
		{Voice: "https://models.example/archive.zip"},
		{Voice: "voice", ModelURL: "https://models.example/voice.onnx", ConfigURL: "file:///private/config.json"},
		{Voice: "voice", ModelURL: "https://models.example/voice.onnx", ModelSHA256: "bad-checksum"},
	} {
		if _, err := ResolveAdministratorVoice(request); err == nil {
			t.Errorf("invalid administrator source accepted: %+v", request)
		}
	}
}

type testVoiceSource struct {
	server        *httptest.Server
	mu            sync.Mutex
	model, config []byte
	block         <-chan struct{}
	entered       chan struct{}
	requests      atomic.Int64
}

func newTestVoiceSource(t *testing.T) *testVoiceSource {
	t.Helper()
	s := &testVoiceSource{model: []byte("model-one"), config: []byte(`{"audio":{"sample_rate":22050}}`), entered: make(chan struct{}, 1)}
	s.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		s.mu.Lock()
		body := append([]byte(nil), s.model...)
		block := s.block
		if r.URL.Path == "/voice.onnx.json" {
			body = append([]byte(nil), s.config...)
		}
		s.mu.Unlock()
		if r.URL.Path == "/voice.onnx.json" && block != nil {
			select {
			case s.entered <- struct{}{}:
			default:
			}
			select {
			case <-block:
			case <-r.Context().Done():
				return
			}
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(s.server.Close)
	return s
}
func (s *testVoiceSource) spec(t *testing.T) VoiceSpec {
	t.Helper()
	spec, err := ResolveAdministratorVoice(AdministratorVoiceRequest{Voice: s.server.URL + "/voice.onnx"})
	if err != nil {
		t.Fatal(err)
	}
	return spec
}
func digest(data []byte) string { hash := sha256.Sum256(data); return hex.EncodeToString(hash[:]) }

func TestAdministratorInstallationPublishesDigestIdentifiedPairAndPreservesReaders(t *testing.T) {
	source := newTestVoiceSource(t)
	root := t.TempDir()
	installer := AdministratorVoiceInstaller{Client: source.server.Client()}
	if err := installer.Install(context.Background(), root, source.spec(t)); err != nil {
		t.Fatal(err)
	}
	old, ok := InstalledVoicePath(root, "voice")
	if !ok {
		t.Fatal("unlisted administrator voice not installed")
	}
	manifestBytes, err := os.ReadFile(activeManifestPath(root, "voice"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest activeVoiceManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil || manifest.ModelSHA256 != digest(source.model) || manifest.ConfigSHA256 != digest(source.config) {
		t.Fatalf("unverified pair identity: %+v %v", manifest, err)
	}
	if err := InstallVoice(context.Background(), root, "voice"); err == nil {
		t.Fatal("ordinary fixed-catalog installer gained administrator override")
	}
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	source.mu.Lock()
	source.model = []byte("model-two")
	source.block = release
	source.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- installer.Install(context.Background(), root, source.spec(t)) }()
	select {
	case <-source.entered:
	case <-time.After(time.Second):
		t.Fatal("replacement sidecar did not reach barrier")
	}
	if current, ok := InstalledVoicePath(root, "voice"); !ok || current != old {
		t.Fatal("partial replacement became visible", current, ok)
	}
	unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement did not finish")
	}
	current, ok := InstalledVoicePath(root, "voice")
	if !ok || current == old {
		t.Fatal("replacement not published")
	}
	if body, err := os.ReadFile(old); err != nil || string(body) != "model-one" {
		t.Fatal("existing reader lost its immutable model", string(body), err)
	}
	if spec, ok := ResolveVoice(root, "voice"); !ok || spec.ModelURL != "" {
		t.Fatal("installed voice still has automatic download metadata", spec, ok)
	}
}

func TestAdministratorFailuresAndCancellationPreserveActiveVoice(t *testing.T) {
	for _, mode := range []string{"checksum", "metadata", "piper", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			source := newTestVoiceSource(t)
			root := t.TempDir()
			installer := AdministratorVoiceInstaller{Client: source.server.Client()}
			if err := installer.Install(context.Background(), root, source.spec(t)); err != nil {
				t.Fatal(err)
			}
			old, ok := InstalledVoicePath(root, "voice")
			if !ok {
				t.Fatal("initial pair missing")
			}
			source.mu.Lock()
			source.model = []byte("replacement")
			if mode == "metadata" {
				source.config = []byte("invalid JSON")
			}
			source.mu.Unlock()
			spec := source.spec(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "checksum":
				spec.ModelSHA256 = strings.Repeat("0", 64)
			case "piper":
				installer.Validate = func(context.Context, string) error { return errors.New("fixture Piper rejected model") }
			case "cancel":
				installer.Validate = func(context.Context, string) error { cancel(); return nil }
			}
			if err := installer.Install(ctx, root, spec); err == nil {
				t.Fatal("invalid/canceled replacement accepted")
			}
			if current, ok := InstalledVoicePath(root, "voice"); !ok || current != old {
				t.Fatal("failure changed active voice", current, ok)
			}
			entries, err := os.ReadDir(filepath.Join(root, voiceVersionsDir, "voice"))
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".voice-install-") {
					t.Fatal("partial installation remains", entry.Name())
				}
			}
		})
	}
}

func TestAdministratorDownloadsBlockPrivateEndpointsAndHTTPSDowngrade(t *testing.T) {
	source := newTestVoiceSource(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := (AdministratorVoiceInstaller{}).Install(ctx, t.TempDir(), source.spec(t)); err == nil || !strings.Contains(err.Error(), "disallowed") {
		t.Fatal("production transport admitted private destination", err)
	}
	if source.requests.Load() != 0 {
		t.Fatal("private endpoint received a request")
	}
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://models.example/voice.onnx", http.StatusFound)
	}))
	defer redirect.Close()
	client := administratorDownloadClient()
	client.Transport = redirect.Client().Transport // fixture CA/dial only, retain production redirect policy
	defer client.CloseIdleConnections()
	spec, err := ResolveAdministratorVoice(AdministratorVoiceRequest{Voice: redirect.URL + "/voice.onnx"})
	if err != nil {
		t.Fatal(err)
	}
	if err := (AdministratorVoiceInstaller{Client: client}).Install(context.Background(), t.TempDir(), spec); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatal("download followed HTTPS downgrade", err)
	}
}

func TestAdministratorSidecarDownloadHasSeparateSizeBudget(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/voice.onnx" {
			_, _ = w.Write([]byte("model"))
			return
		}
		_, _ = io.CopyN(w, strings.NewReader(strings.Repeat("x", 16<<20)), 16<<20)
	}))
	defer server.Close()
	spec, err := ResolveAdministratorVoice(AdministratorVoiceRequest{Voice: server.URL + "/voice.onnx"})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := (AdministratorVoiceInstaller{Client: server.Client()}).Install(context.Background(), root, spec); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatal("oversized sidecar was admitted", err)
	}
	if IsInstalledVoice(root, "voice") {
		t.Fatal("oversized metadata became installed")
	}
}

func TestInstalledAdministratorOverrideOfBundledNameDoesNotAutoDownload(t *testing.T) {
	root := t.TempDir()
	model := filepath.Join(root, BundledPromptVoice+".onnx")
	if err := os.WriteFile(model, []byte("operator-customized-model"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(model+".json", []byte(`{"audio":{"sample_rate":22050}}`), 0600); err != nil {
		t.Fatal(err)
	}
	voice, ok := ResolveVoice(root, BundledPromptVoice)
	if !ok || voice.ModelURL != "" {
		t.Fatal("selecting the administrator's installed override would redownload the catalog", voice, ok)
	}
}
