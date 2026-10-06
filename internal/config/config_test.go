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
		"VOXMAIL_GREETING_PATH", "VOXMAIL_BARESIP_BINARY", "VOXMAIL_BARESIP_CONFIG", "VOXMAIL_TRUSTED_PROXY_CIDRS",
		"VOXMAIL_HISTORY_RETENTION_DAYS", "VOXMAIL_HISTORY_MAX_ROWS",
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
	if c.HistoryRetentionDays != 365 || c.HistoryMaxRows != 100000 {
		t.Errorf("history defaults = %d days/%d rows", c.HistoryRetentionDays, c.HistoryMaxRows)
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
	t.Setenv("VOXMAIL_HISTORY_RETENTION_DAYS", "90")
	t.Setenv("VOXMAIL_HISTORY_MAX_ROWS", "5000")
	t.Setenv("VOXMAIL_TRUSTED_PROXY_CIDRS", "203.0.113.0/24, 2001:db8::/32,203.0.113.0/24")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.DataDir != "/custom/data" || c.HTTPAddr != ":9999" || c.MaxCalls != 7 || c.HistoryRetentionDays != 90 || c.HistoryMaxRows != 5000 {
		t.Errorf("custom values not applied: %+v", c)
	}
	if want := "/custom/data/sqlite/voxmail.db"; c.DBPath != want {
		t.Errorf("DBPath = %q, want %q", c.DBPath, want)
	}
	if len(c.TrustedProxyCIDRs) != 2 || c.TrustedProxyCIDRs[0] != "203.0.113.0/24" || c.TrustedProxyCIDRs[1] != "2001:db8::/32" {
		t.Fatalf("trusted proxy CIDRs = %v", c.TrustedProxyCIDRs)
	}
}

func TestLoadRejectsInvalidTrustedProxyCIDR(t *testing.T) {
	clearEnv(t)
	t.Setenv("VOXMAIL_ENCRYPTION_KEY", "01234567890123456789012345678901")
	t.Setenv("VOXMAIL_TRUSTED_PROXY_CIDRS", "not-a-cidr")
	if _, err := Load(); err == nil {
		t.Fatal("Load accepted an invalid trusted proxy CIDR")
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

func TestLoadRejectsInvalidHistoryRetention(t *testing.T) {
	clearEnv(t)
	t.Setenv("VOXMAIL_ENCRYPTION_KEY", "01234567890123456789012345678901")
	t.Setenv("VOXMAIL_HISTORY_RETENTION_DAYS", "0")
	if _, err := Load(); err == nil {
		t.Fatal("Load accepted zero history retention")
	}
	clearEnv(t)
	t.Setenv("VOXMAIL_ENCRYPTION_KEY", "01234567890123456789012345678901")
	t.Setenv("VOXMAIL_HISTORY_MAX_ROWS", "99")
	if _, err := Load(); err == nil {
		t.Fatal("Load accepted history row cap below the safety minimum")
	}
}
