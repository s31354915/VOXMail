package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config contains deployment-level settings. User and account settings live in SQLite.
type Config struct {
	HTTPAddr             string
	DataDir              string
	DBPath               string
	EncryptionKey        string
	ControlSocket        string
	MaxCalls             int
	STTBinary            string
	STTModel             string
	PiperBinary          string
	PiperModel           string
	VoiceDir             string
	RecordingsDir        string
	GreetingPath         string
	BaresipBinary        string
	BaresipConfig        string
	TrustedProxyCIDRs    []string
	HistoryRetentionDays int
	HistoryMaxRows       int
}

func Load() (Config, error) {
	dataDir := env("VOXMAIL_DATA_DIR", "/data")
	trustedProxyCIDRs, err := parseCIDRs(os.Getenv("VOXMAIL_TRUSTED_PROXY_CIDRS"))
	if err != nil {
		return Config{}, err
	}
	c := Config{
		HTTPAddr:             env("VOXMAIL_HTTP_ADDR", "127.0.0.1:8080"),
		DataDir:              dataDir,
		DBPath:               env("VOXMAIL_DB_PATH", filepath.Join(dataDir, "sqlite", "voxmail.db")),
		EncryptionKey:        os.Getenv("VOXMAIL_ENCRYPTION_KEY"),
		ControlSocket:        env("VOXMAIL_CONTROL_SOCKET", filepath.Join(dataDir, "run", "baresip.sock")),
		MaxCalls:             envInt("VOXMAIL_MAX_CALLS", 10),
		STTBinary:            env("VOXMAIL_STT_BINARY", "whisper-cli"),
		STTModel:             env("VOXMAIL_STT_MODEL", filepath.Join(dataDir, "whisper", "ggml-base.en.bin")),
		PiperBinary:          env("VOXMAIL_PIPER_BINARY", "piper"),
		PiperModel:           env("VOXMAIL_PIPER_MODEL", filepath.Join(dataDir, "voices", "en_US-hfc_male-medium.onnx")),
		VoiceDir:             env("VOXMAIL_VOICE_DIR", filepath.Join(dataDir, "voices")),
		RecordingsDir:        env("VOXMAIL_RECORDINGS_DIR", filepath.Join(dataDir, "recordings")),
		GreetingPath:         env("VOXMAIL_GREETING_PATH", filepath.Join(dataDir, "prompts", "welcome.wav")),
		BaresipBinary:        env("VOXMAIL_BARESIP_BINARY", "baresip"),
		BaresipConfig:        env("VOXMAIL_BARESIP_CONFIG", filepath.Join(dataDir, "config", "baresip")),
		TrustedProxyCIDRs:    trustedProxyCIDRs,
		HistoryRetentionDays: envInt("VOXMAIL_HISTORY_RETENTION_DAYS", 365),
		HistoryMaxRows:       envInt("VOXMAIL_HISTORY_MAX_ROWS", 100000),
	}
	if c.MaxCalls < 1 || c.MaxCalls > 100 {
		return Config{}, fmt.Errorf("VOXMAIL_MAX_CALLS must be between 1 and 100")
	}
	if c.HistoryRetentionDays < 1 || c.HistoryRetentionDays > 3650 {
		return Config{}, fmt.Errorf("VOXMAIL_HISTORY_RETENTION_DAYS must be between 1 and 3650")
	}
	if c.HistoryMaxRows < 100 || c.HistoryMaxRows > 10000000 {
		return Config{}, fmt.Errorf("VOXMAIL_HISTORY_MAX_ROWS must be between 100 and 10000000")
	}
	if c.EncryptionKey == "" {
		return Config{}, fmt.Errorf("VOXMAIL_ENCRYPTION_KEY is required")
	}
	if len(c.EncryptionKey) < 32 {
		return Config{}, fmt.Errorf("VOXMAIL_ENCRYPTION_KEY must be at least 32 bytes")
	}
	return c, nil
}

func parseCIDRs(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	seen := make(map[string]struct{})
	var result []string
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		_, network, err := net.ParseCIDR(item)
		if err != nil {
			return nil, fmt.Errorf("VOXMAIL_TRUSTED_PROXY_CIDRS contains invalid CIDR %q: %w", item, err)
		}
		canonical := network.String()
		if _, exists := seen[canonical]; exists {
			continue
		}
		seen[canonical] = struct{}{}
		result = append(result, canonical)
	}
	return result, nil
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) int {
	value, err := strconv.Atoi(env(key, strconv.Itoa(fallback)))
	if err != nil {
		return fallback
	}
	return value
}
