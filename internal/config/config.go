package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// Config contains deployment-level settings. User and account settings live in SQLite.
type Config struct {
	HTTPAddr      string
	DataDir       string
	DBPath        string
	EncryptionKey string
	ControlSocket string
	MaxCalls      int
	STTBinary     string
	STTModel      string
	PiperBinary   string
	PiperModel    string
	VoiceDir      string
	RecordingsDir string
	BaresipBinary string
	BaresipConfig string
}

func Load() (Config, error) {
	dataDir := env("VOXMAIL_DATA_DIR", "/data")
	c := Config{
		HTTPAddr:      env("VOXMAIL_HTTP_ADDR", ":8080"),
		DataDir:       dataDir,
		DBPath:        env("VOXMAIL_DB_PATH", filepath.Join(dataDir, "sqlite", "voxmail.db")),
		EncryptionKey: os.Getenv("VOXMAIL_ENCRYPTION_KEY"),
		ControlSocket: env("VOXMAIL_CONTROL_SOCKET", filepath.Join(dataDir, "run", "baresip.sock")),
		MaxCalls:      envInt("VOXMAIL_MAX_CALLS", 10),
		STTBinary:     env("VOXMAIL_STT_BINARY", "whisper-cli"),
		STTModel:      env("VOXMAIL_STT_MODEL", filepath.Join(dataDir, "whisper", "ggml-base.en.bin")),
		PiperBinary:   env("VOXMAIL_PIPER_BINARY", "piper"),
		PiperModel:    env("VOXMAIL_PIPER_MODEL", filepath.Join(dataDir, "voices", "en_US-hfc_male-medium.onnx")),
		VoiceDir:      env("VOXMAIL_VOICE_DIR", filepath.Join(dataDir, "voices")),
		RecordingsDir: env("VOXMAIL_RECORDINGS_DIR", filepath.Join(dataDir, "recordings")),
		BaresipBinary: env("VOXMAIL_BARESIP_BINARY", "baresip"),
		BaresipConfig: env("VOXMAIL_BARESIP_CONFIG", filepath.Join(dataDir, "config", "baresip")),
	}
	if c.MaxCalls < 1 || c.MaxCalls > 100 {
		return Config{}, fmt.Errorf("VOXMAIL_MAX_CALLS must be between 1 and 100")
	}
	if c.EncryptionKey == "" {
		return Config{}, fmt.Errorf("VOXMAIL_ENCRYPTION_KEY is required")
	}
	if len(c.EncryptionKey) < 32 {
		return Config{}, fmt.Errorf("VOXMAIL_ENCRYPTION_KEY must be at least 32 bytes")
	}
	return c, nil
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
