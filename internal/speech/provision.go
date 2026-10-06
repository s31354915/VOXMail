package speech

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const (
	PiperModelURL    = "https://huggingface.co/rhasspy/piper-voices/resolve/v1.0.0/en/en_US/hfc_male/medium/en_US-hfc_male-medium.onnx?download=true"
	PiperConfigURL   = "https://huggingface.co/rhasspy/piper-voices/resolve/v1.0.0/en/en_US/hfc_male/medium/en_US-hfc_male-medium.onnx.json?download=true"
	WhisperBaseENURL = "https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-base.en.bin?download=true"

	PiperModelSHA256    = "d11e403a02bdf5a670c877b3dc56e0e1c8cece6fb30289586314dffdc0a78cb0"
	PiperConfigSHA256   = "f66847424aed0bf99ecbb5d7cfde47c0a906f426a0daf7c46f305e7d21afd886"
	WhisperBaseENSHA256 = "a03779c86df3323075f5e796cb2ce5029f00ec8869eee3fdfb897afe36c6d002"
)

type VoiceSpec struct {
	Voice        string `json:"voice"`
	ModelURL     string `json:"-"`
	ConfigURL    string `json:"-"`
	ModelSHA256  string `json:"-"`
	ConfigSHA256 string `json:"-"`
}

const (
	voiceVersionsDir = ".versions"
	voiceActiveDir   = ".active"
)

type activeVoiceManifest struct {
	Version      int    `json:"version"`
	Voice        string `json:"voice"`
	Model        string `json:"model"`
	Config       string `json:"config"`
	ModelSHA256  string `json:"model_sha256"`
	ConfigSHA256 string `json:"config_sha256"`
}

type voiceOperationLock struct{ gate chan struct{} }

var voiceOperationLocks sync.Map

// VoiceOperation serializes installation and activation of one model. The
// lock is process-wide for this VOXMail instance, and the callback-style API
// keeps the lock held across download, validation, and pointer publication.
type VoiceOperation struct {
	voiceDir string
	name     string
	lock     *voiceOperationLock
}

// AcquireVoiceOperation obtains the per-model operation lock with context
// cancellation. Call Close exactly once when the operation is complete.
func AcquireVoiceOperation(ctx context.Context, voiceDir, name string) (*VoiceOperation, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	name, ok := normalizeVoiceName(name)
	if !ok || strings.TrimSpace(voiceDir) == "" {
		return nil, fmt.Errorf("invalid voice operation")
	}
	key := filepath.Clean(voiceDir) + "\x00" + name
	value, _ := voiceOperationLocks.LoadOrStore(key, &voiceOperationLock{gate: make(chan struct{}, 1)})
	lock := value.(*voiceOperationLock)
	select {
	case lock.gate <- struct{}{}:
		return &VoiceOperation{voiceDir: voiceDir, name: name, lock: lock}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Close releases the per-model operation lock.
func (o *VoiceOperation) Close() {
	if o == nil || o.lock == nil {
		return
	}
	select {
	case <-o.lock.gate:
	default:
	}
	o.lock = nil
}

func (o *VoiceOperation) InstallVoice(ctx context.Context) error {
	if o == nil || o.lock == nil {
		return fmt.Errorf("voice operation is closed")
	}
	voice, ok := FindTrustedVoice(o.name)
	if !ok {
		return fmt.Errorf("voice is not in the trusted catalog")
	}
	return installVoiceLocked(ctx, o.voiceDir, voice)
}

// normalizeVoiceName accepts the basename form used by Piper and rejects
// anything that could escape the configured voice directory.  Trusted voices
// are still resolved from the fixed catalog; this validation only governs
// locally installed model names.
func normalizeVoiceName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if strings.HasSuffix(name, ".onnx") {
		name = strings.TrimSuffix(name, ".onnx")
	}
	if name == "" || filepath.Base(name) != name || strings.ContainsAny(name, "/\\\x00\r\n") || name == "." || name == ".." {
		return "", false
	}
	return name, true
}

// TrustedVoiceCatalog is intentionally explicit. Adding a voice is a source
// code/deployment change, not a URL supplied by a browser user.
func TrustedVoiceCatalog() []VoiceSpec {
	return []VoiceSpec{{Voice: "en_US-hfc_male-medium", ModelURL: PiperModelURL, ConfigURL: PiperConfigURL, ModelSHA256: PiperModelSHA256, ConfigSHA256: PiperConfigSHA256}}
}

func FindTrustedVoice(name string) (VoiceSpec, bool) {
	name, ok := normalizeVoiceName(name)
	if !ok {
		return VoiceSpec{}, false
	}
	for _, voice := range TrustedVoiceCatalog() {
		if voice.Voice == name {
			return voice, true
		}
	}
	return VoiceSpec{}, false
}

func activeManifestPath(voiceDir, name string) string {
	return filepath.Join(voiceDir, voiceActiveDir, name+".json")
}

func versionDir(voiceDir, name, version string) string {
	return filepath.Join(voiceDir, voiceVersionsDir, name, version)
}

func safeVoiceRelativePath(root, relative string) (string, bool) {
	if relative == "" || filepath.IsAbs(relative) {
		return "", false
	}
	clean := filepath.Clean(filepath.FromSlash(relative))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", false
	}
	joined := filepath.Join(root, clean)
	within, err := filepath.Rel(filepath.Clean(root), joined)
	if err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
		return "", false
	}
	return joined, true
}

func activeVoiceArtifactPath(voiceDir, name, relative, filename string) (string, string, bool) {
	clean := filepath.Clean(filepath.FromSlash(relative))
	parts := strings.Split(filepath.ToSlash(clean), "/")
	if len(parts) != 4 || parts[0] != voiceVersionsDir || parts[1] != name || parts[3] != filename {
		return "", "", false
	}
	if parts[2] == "" || parts[2] == "." || parts[2] == ".." || strings.ContainsAny(parts[2], "/\\") {
		return "", "", false
	}
	path, ok := safeVoiceRelativePath(voiceDir, relative)
	if !ok {
		return "", "", false
	}
	return path, parts[2], true
}

func realVersionDirectory(voiceDir, name, version string) bool {
	for _, path := range []string{
		filepath.Join(voiceDir, voiceVersionsDir),
		filepath.Join(voiceDir, voiceVersionsDir, name),
		filepath.Join(voiceDir, voiceVersionsDir, name, version),
	} {
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return false
		}
	}
	return true
}

func usableVoicePair(model, config, expectedModel, expectedConfig string) bool {
	modelInfo, err := os.Lstat(model)
	if err != nil || !modelInfo.Mode().IsRegular() || modelInfo.Size() == 0 {
		return false
	}
	configInfo, err := os.Lstat(config)
	if err != nil || !configInfo.Mode().IsRegular() || configInfo.Size() == 0 || !validPiperConfig(config) {
		return false
	}
	return (expectedModel == "" || artifactMatches(model, expectedModel)) && (expectedConfig == "" || artifactMatches(config, expectedConfig))
}

// InstalledVoicePath returns the immutable model selected by the active
// pointer. Legacy direct pairs remain readable for upgrades, but a malformed
// active pointer never falls back to them because that could expose a stale or
// mismatched pair.
func InstalledVoicePath(voiceDir, name string) (string, bool) {
	name, ok := normalizeVoiceName(name)
	if !ok || strings.TrimSpace(voiceDir) == "" {
		return "", false
	}
	manifestPath := activeManifestPath(voiceDir, name)
	data, err := os.ReadFile(manifestPath)
	if err == nil {
		var manifest activeVoiceManifest
		if json.Unmarshal(data, &manifest) != nil || manifest.Version != 1 || manifest.Voice != name {
			return "", false
		}
		model, modelVersion, modelOK := activeVoiceArtifactPath(voiceDir, name, manifest.Model, name+".onnx")
		config, configVersion, configOK := activeVoiceArtifactPath(voiceDir, name, manifest.Config, name+".onnx.json")
		if !modelOK || !configOK || modelVersion != configVersion || !realVersionDirectory(voiceDir, name, modelVersion) ||
			!validSHA256(manifest.ModelSHA256) || !validSHA256(manifest.ConfigSHA256) ||
			!usableVoicePair(model, config, manifest.ModelSHA256, manifest.ConfigSHA256) {
			return "", false
		}
		return model, true
	}
	if !os.IsNotExist(err) {
		return "", false
	}
	model := filepath.Join(voiceDir, name+".onnx")
	return model, usableVoicePair(model, model+".json", "", "")
}

// IsInstalledVoice verifies both Piper artifacts. A model is not considered
// installed when its sidecar is absent, malformed, empty, or mismatched with
// the active pointer.
func IsInstalledVoice(voiceDir, name string) bool {
	_, ok := InstalledVoicePath(voiceDir, name)
	return ok
}

// InstalledVoiceNames returns safe, usable local Piper model names in stable
// order.  It intentionally does not discover URLs or download anything.
func InstalledVoiceNames(voiceDir string) []string {
	if strings.TrimSpace(voiceDir) == "" {
		return nil
	}
	entries, err := os.ReadDir(voiceDir)
	if err != nil {
		return nil
	}
	seen := make(map[string]struct{})
	voices := make([]string, 0)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".onnx") {
			continue
		}
		name, ok := normalizeVoiceName(strings.TrimSuffix(entry.Name(), ".onnx"))
		if !ok || !IsInstalledVoice(voiceDir, name) {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		voices = append(voices, name)
	}
	activeDir := filepath.Join(voiceDir, voiceActiveDir)
	if activeEntries, readErr := os.ReadDir(activeDir); readErr == nil {
		for _, entry := range activeEntries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			name, ok := normalizeVoiceName(strings.TrimSuffix(entry.Name(), ".json"))
			if !ok || !IsInstalledVoice(voiceDir, name) {
				continue
			}
			if _, exists := seen[name]; exists {
				continue
			}
			seen[name] = struct{}{}
			voices = append(voices, name)
		}
	}
	sort.Strings(voices)
	return voices
}

// ResolveConfiguredModel upgrades the legacy direct default path to the
// active immutable model when the configured path names a voice in voiceDir.
// Explicit custom paths outside voiceDir remain untouched.
func ResolveConfiguredModel(voiceDir, configured string) string {
	if strings.TrimSpace(voiceDir) == "" || strings.TrimSpace(configured) == "" {
		return configured
	}
	if filepath.Clean(filepath.Dir(configured)) != filepath.Clean(voiceDir) {
		return configured
	}
	name := strings.TrimSuffix(filepath.Base(configured), filepath.Ext(configured))
	if model, ok := InstalledVoicePath(voiceDir, name); ok {
		return model
	}
	return configured
}

// ResolveVoice accepts either a catalog voice (which may be installed by the
// caller) or a model already present in the configured directory.  Local
// voices have no download metadata, so browser input can never turn this into
// an arbitrary remote download.
func ResolveVoice(voiceDir, name string) (VoiceSpec, bool) {
	if voice, ok := FindTrustedVoice(name); ok {
		return voice, true
	}
	name, ok := normalizeVoiceName(name)
	if !ok || !IsInstalledVoice(voiceDir, name) {
		return VoiceSpec{}, false
	}
	return VoiceSpec{Voice: name}, true
}

func writeActiveVoiceManifest(voiceDir string, manifest activeVoiceManifest) error {
	if err := os.MkdirAll(filepath.Join(voiceDir, voiceActiveDir), 0700); err != nil {
		return err
	}
	path := activeManifestPath(voiceDir, manifest.Voice)
	tmp, err := os.CreateTemp(filepath.Dir(path), ".active-*.json")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	published := false
	defer func() {
		if !published {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := json.NewEncoder(tmp).Encode(manifest); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	published = true
	return nil
}

func preserveVoicePath(path, root, prefix string) (string, error) {
	recovery, err := os.MkdirTemp(root, prefix)
	if err != nil {
		return "", err
	}
	if err := os.Remove(recovery); err != nil {
		return "", err
	}
	if err := os.Rename(path, recovery); err != nil {
		return "", err
	}
	return recovery, nil
}

func copyArtifact(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		return err
	}
	return output.Close()
}

func InstallVoice(ctx context.Context, voiceDir, name string) error {
	voice, ok := FindTrustedVoice(name)
	if !ok {
		return fmt.Errorf("voice is not in the trusted catalog")
	}
	operation, err := AcquireVoiceOperation(ctx, voiceDir, voice.Voice)
	if err != nil {
		return err
	}
	defer operation.Close()
	return operation.InstallVoice(ctx)
}

func installVoiceLocked(ctx context.Context, voiceDir string, voice VoiceSpec) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := os.MkdirAll(voiceDir, 0700); err != nil {
		return err
	}
	version := voice.ModelSHA256 + "-" + voice.ConfigSHA256
	versionRoot := filepath.Join(voiceDir, voiceVersionsDir, voice.Voice)
	target := versionDir(voiceDir, voice.Voice, version)
	targetModel := filepath.Join(target, voice.Voice+".onnx")
	targetConfig := targetModel + ".json"
	activate := func() error {
		return writeActiveVoiceManifest(voiceDir, activeVoiceManifest{
			Version:      1,
			Voice:        voice.Voice,
			Model:        filepath.ToSlash(filepath.Join(voiceVersionsDir, voice.Voice, version, filepath.Base(targetModel))),
			Config:       filepath.ToSlash(filepath.Join(voiceVersionsDir, voice.Voice, version, filepath.Base(targetConfig))),
			ModelSHA256:  voice.ModelSHA256,
			ConfigSHA256: voice.ConfigSHA256,
		})
	}
	if usableVoicePair(targetModel, targetConfig, voice.ModelSHA256, voice.ConfigSHA256) {
		if err := activate(); err != nil {
			return fmt.Errorf("activate voice pointer (version retained at %s): %w", target, err)
		}
		return nil
	}
	if err := os.MkdirAll(versionRoot, 0700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(versionRoot, ".voice-install-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	stageModel := filepath.Join(stage, filepath.Base(targetModel))
	stageConfig := filepath.Join(stage, filepath.Base(targetConfig))
	legacyModel := filepath.Join(voiceDir, voice.Voice+".onnx")
	legacyConfig := legacyModel + ".json"
	if artifactMatches(legacyModel, voice.ModelSHA256) && artifactMatches(legacyConfig, voice.ConfigSHA256) && validPiperConfig(legacyConfig) {
		if err := copyArtifact(legacyModel, stageModel); err != nil {
			return err
		}
		if err := copyArtifact(legacyConfig, stageConfig); err != nil {
			return err
		}
	} else {
		if err := downloadVerified(ctx, voice.ModelURL, stageModel, voice.ModelSHA256); err != nil {
			return err
		}
		if err := downloadVerified(ctx, voice.ConfigURL, stageConfig, voice.ConfigSHA256); err != nil {
			return err
		}
	}
	if !usableVoicePair(stageModel, stageConfig, voice.ModelSHA256, voice.ConfigSHA256) {
		return fmt.Errorf("voice sidecar is not valid JSON")
	}
	if _, err := os.Stat(target); err == nil {
		if recovery, preserveErr := preserveVoicePath(target, versionRoot, ".voice-version-failed-"); preserveErr != nil {
			stageRecovery, stageErr := preserveVoicePath(stage, versionRoot, ".voice-install-failed-")
			if stageErr == nil {
				return fmt.Errorf("preserve incomplete voice version %s: %w; staged recovery retained at %s", target, preserveErr, stageRecovery)
			}
			return fmt.Errorf("preserve incomplete voice version %s: %w; staged recovery could not be retained: %v", target, preserveErr, stageErr)
		} else {
			_ = recovery
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(stage, target); err != nil {
		if recovery, preserveErr := preserveVoicePath(stage, versionRoot, ".voice-install-failed-"); preserveErr == nil {
			return fmt.Errorf("publish voice version %s: %w; staged recovery retained at %s", target, err, recovery)
		}
		return fmt.Errorf("publish voice version %s: %w; staged recovery could not be retained", target, err)
	}
	if err := activate(); err != nil {
		return fmt.Errorf("activate voice pointer (version retained at %s): %w", target, err)
	}
	return nil
}

// maxModelDownload is a safety ceiling for anything Provision may fetch. The
// largest legitimate artifact is whisper.cpp's ggml-base.en.bin at roughly
// 150MB, so 4GiB only guards against a server returning garbage.
const maxModelDownload = 4 << 30

// Provision downloads only missing model files, writing each file atomically.
// It is opt-in because model downloads are large and should be visible in
// deployment logs.
func Provision(ctx context.Context, voiceDir, whisperPath string) error {
	if err := InstallVoice(ctx, voiceDir, BundledPromptVoice); err != nil {
		return err
	}
	if artifactMatches(whisperPath, WhisperBaseENSHA256) {
		return nil
	}
	return downloadVerified(ctx, WhisperBaseENURL, whisperPath, WhisperBaseENSHA256)
}

func download(ctx context.Context, url, destination string) error {
	return downloadVerified(ctx, url, destination, "")
}

func downloadVerified(ctx context.Context, url, destination, expectedSHA256 string) error {
	if info, err := os.Stat(destination); err == nil && info.Size() > 0 {
		if expectedSHA256 == "" || artifactMatches(destination, expectedSHA256) {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return fmt.Errorf("download model: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download model: HTTP %s", resp.Status)
	}
	tmp, err := os.CreateTemp(filepath.Dir(destination), ".model-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, hash), io.LimitReader(resp.Body, maxModelDownload))
	if err != nil {
		tmp.Close()
		return err
	}
	if n >= maxModelDownload {
		tmp.Close()
		return fmt.Errorf("download model: file exceeds size limit")
	}
	if expectedSHA256 != "" && hex.EncodeToString(hash.Sum(nil)) != strings.ToLower(strings.TrimSpace(expectedSHA256)) {
		tmp.Close()
		return fmt.Errorf("download model: SHA-256 checksum mismatch")
	}
	if err = tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, destination)
}

func artifactMatches(path, expected string) bool {
	expected = strings.ToLower(strings.TrimSpace(expected))
	if expected == "" {
		return false
	}
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(file, maxModelDownload)); err != nil {
		return false
	}
	return hex.EncodeToString(hash.Sum(nil)) == expected
}

func validSHA256(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validPiperConfig(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return false
	}
	var document map[string]any
	return json.Unmarshal(data, &document) == nil && len(document) > 0
}

// ValidPiperConfig reports whether a sidecar is a non-empty JSON document.
// It is used by the runtime activator after verified installation so an
// orphaned or truncated sidecar cannot be treated as a usable voice.
func ValidPiperConfig(path string) bool { return validPiperConfig(path) }
