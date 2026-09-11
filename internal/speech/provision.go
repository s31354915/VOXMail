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

// IsInstalledVoice verifies both Piper artifacts.  A model is not considered
// installed when its sidecar is absent, malformed, empty, or when the model
// itself is empty.  This prevents the web console from advertising a voice
// that Piper cannot actually load.
func IsInstalledVoice(voiceDir, name string) bool {
	name, ok := normalizeVoiceName(name)
	if !ok || strings.TrimSpace(voiceDir) == "" {
		return false
	}
	model := filepath.Join(voiceDir, name+".onnx")
	info, err := os.Lstat(model)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return false
	}
	config := model + ".json"
	configInfo, err := os.Lstat(config)
	return err == nil && configInfo.Mode().IsRegular() && configInfo.Size() > 0 && validPiperConfig(config)
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
	sort.Strings(voices)
	return voices
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

func InstallVoice(ctx context.Context, voiceDir, name string) error {
	voice, ok := FindTrustedVoice(name)
	if !ok {
		return fmt.Errorf("voice is not in the trusted catalog")
	}
	if err := os.MkdirAll(voiceDir, 0700); err != nil {
		return err
	}
	model := filepath.Join(voiceDir, voice.Voice+".onnx")
	config := model + ".json"
	if artifactMatches(model, voice.ModelSHA256) && artifactMatches(config, voice.ConfigSHA256) && validPiperConfig(config) {
		return nil
	}
	stage, err := os.MkdirTemp(voiceDir, ".voice-install-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	stageModel := filepath.Join(stage, filepath.Base(model))
	stageConfig := filepath.Join(stage, filepath.Base(config))
	if err := downloadVerified(ctx, voice.ModelURL, stageModel, voice.ModelSHA256); err != nil {
		return err
	}
	if err := downloadVerified(ctx, voice.ConfigURL, stageConfig, voice.ConfigSHA256); err != nil {
		return err
	}
	if !validPiperConfig(stageConfig) {
		return fmt.Errorf("voice sidecar is not valid JSON")
	}
	if err := atomicInstallPair(stageModel, model, stageConfig, config); err != nil {
		return fmt.Errorf("activate voice model: %w", err)
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

// atomicInstallPair activates the model and sidecar together. The backup and
// rollback path prevents a failed second rename from leaving the active
// directory with mismatched artifacts.
func atomicInstallPair(stagedModel, model, stagedConfig, config string) error {
	backupDir, err := os.MkdirTemp(filepath.Dir(model), ".voice-backup-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(backupDir)
	backupModel := filepath.Join(backupDir, filepath.Base(model))
	backupConfig := filepath.Join(backupDir, filepath.Base(config))
	hadModel := false
	hadConfig := false
	if _, err := os.Stat(model); err == nil {
		if err := os.Rename(model, backupModel); err != nil {
			return err
		}
		hadModel = true
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, err := os.Stat(config); err == nil {
		if err := os.Rename(config, backupConfig); err != nil {
			if hadModel {
				_ = os.Rename(backupModel, model)
			}
			return err
		}
		hadConfig = true
	} else if !os.IsNotExist(err) {
		if hadModel {
			_ = os.Rename(backupModel, model)
		}
		return err
	}
	rollback := func() {
		_ = os.Remove(model)
		_ = os.Remove(config)
		if hadModel {
			_ = os.Rename(backupModel, model)
		}
		if hadConfig {
			_ = os.Rename(backupConfig, config)
		}
	}
	if err := os.Rename(stagedModel, model); err != nil {
		rollback()
		return err
	}
	if err := os.Rename(stagedConfig, config); err != nil {
		rollback()
		return err
	}
	return nil
}
