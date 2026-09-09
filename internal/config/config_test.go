package config

import (
	"path/filepath"
	"testing"
)

func clearEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"VOXMAIL_DATA_DIR", "VOXMAIL_HTTP_ADDR", "VOXMAIL_DB_PATH", "VOXMAIL_ENCRYPTION_KEY",
		"VOXMAIL_CONTROL_SOCKET", "VOXMAIL_MAX_CALLS", "VOXMAIL_STT_BINARY", "VOXMAIL_STT_MODEL",
		"VOXMAIL_PIPER_BINARY", "VOXMAIL_PIPER_MODEL", "VOXMAIL_VOICE_DIR", "VOXMAIL_RECORDINGS_DIR",
		"VOXMAIL_GREETING_PATH", "VOXMAIL_BARESIP_BINARY", "VOXMAIL_BARESIP_CONFIG",
	} {
		t.Setenv(key, "")
	}
}

func TestLoadDefaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("VOXMAIL_ENCRYPTION_KEY", "01234567890123456789012345678901")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.HTTPAddr != "127.0.0.1:8080" {
		t.Errorf("HTTPAddr = %q", c.HTTPAddr)
	}
	if c.MaxCalls != 10 {
		t.Errorf("MaxCalls = %d", c.MaxCalls)
	}
	if want := filepath.Join(c.DataDir, "sqlite", "voxmail.db"); c.DBPath != want {
		t.Errorf("DBPath = %q, want %q", c.DBPath, want)
	}
	if !filepath.IsAbs(c.VoiceDir) {
		t.Errorf("VoiceDir %q should be derived from DataDir", c.VoiceDir)
	}
}

func TestLoadCustom(t *testing.T) {
	clearEnv(t)
	t.Setenv("VOXMAIL_ENCRYPTION_KEY", "01234567890123456789012345678901")
	t.Setenv("VOXMAIL_DATA_DIR", "/custom/data")
	t.Setenv("VOXMAIL_HTTP_ADDR", ":9999")
	t.Setenv("VOXMAIL_MAX_CALLS", "7")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.DataDir != "/custom/data" || c.HTTPAddr != ":9999" || c.MaxCalls != 7 {
		t.Errorf("custom values not applied: %+v", c)
	}
	if want := "/custom/data/sqlite/voxmail.db"; c.DBPath != want {
		t.Errorf("DBPath = %q, want %q", c.DBPath, want)
	}
}

func TestLoadRejectsMissingKey(t *testing.T) {
	clearEnv(t)
	if _, err := Load(); err == nil {
		t.Fatal("Load accepted a missing encryption key")
	}
}

func TestLoadRejectsShortKey(t *testing.T) {
	clearEnv(t)
	t.Setenv("VOXMAIL_ENCRYPTION_KEY", "too-short")
	if _, err := Load(); err == nil {
		t.Fatal("Load accepted a short encryption key")
	}
}

func TestLoadRejectsInvalidMaxCalls(t *testing.T) {
	clearEnv(t)
	t.Setenv("VOXMAIL_ENCRYPTION_KEY", "01234567890123456789012345678901")
	t.Setenv("VOXMAIL_MAX_CALLS", "0")
	if _, err := Load(); err == nil {
		t.Fatal("Load accepted MaxCalls outside 1..100")
	}
	clearEnv(t)
	t.Setenv("VOXMAIL_ENCRYPTION_KEY", "01234567890123456789012345678901")
	t.Setenv("VOXMAIL_MAX_CALLS", "9999")
	if _, err := Load(); err == nil {
		t.Fatal("Load accepted MaxCalls above 100")
	}
}
