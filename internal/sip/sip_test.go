package sip

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/voxmail/voxmail/internal/store"
)

func TestBaresipProcessOutlivesApplyContext(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "fake-baresip.sh")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexec sleep 5\n"), 0700); err != nil {
		t.Fatal(err)
	}
	b := &Baresip{Binary: binary, ConfigDir: filepath.Join(dir, "config"), ControlSocket: filepath.Join(dir, "baresip.sock"), enabled: true}
	ctx, cancel := context.WithCancel(context.Background())
	b.mu.Lock()
	if err := b.startLocked(ctx); err != nil {
		b.mu.Unlock()
		t.Fatalf("startLocked: %v", err)
	}
	cancel()
	b.mu.Unlock()
	time.Sleep(100 * time.Millisecond)
	b.mu.Lock()
	alive := b.cmd != nil && b.cmd.ProcessState == nil
	b.mu.Unlock()
	if !alive {
		b.Stop()
		t.Fatal("baresip process was tied to the Apply request context")
	}
	b.Stop()
}

func TestBaresipRespawnsAfterUnexpectedExit(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "fake-baresip-exit.sh")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexec sh -c 'sleep 0.05; exit 1'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	b := &Baresip{Binary: binary, ConfigDir: filepath.Join(dir, "config"), ControlSocket: filepath.Join(dir, "baresip.sock"), enabled: true}
	b.mu.Lock()
	if err := b.startLocked(context.Background()); err != nil {
		b.mu.Unlock()
		t.Fatalf("startLocked: %v", err)
	}
	first := b.cmd
	b.mu.Unlock()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		respawned := b.cmd != nil && b.cmd != first
		b.mu.Unlock()
		if respawned {
			b.Stop()
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	b.Stop()
	t.Fatal("baresip supervisor did not respawn after an unexpected exit")
}

func TestBuildAccount(t *testing.T) {
	account := buildAccount(store.SIPSettings{Username: "alice", Password: "s3cret", Domain: "sip.example.com", Port: 5060, Transport: "udp", RegInterval: 120})
	if !strings.HasPrefix(account, "<sip:alice@sip.example.com:5060;transport=udp>") {
		t.Fatalf("unexpected account prefix: %s", account)
	}
	if !strings.Contains(account, "auth_pass=s3cret") {
		t.Fatalf("password missing from account: %s", account)
	}
	if !strings.Contains(account, "regint=120") {
		t.Fatalf("regint missing: %s", account)
	}
}

func TestBuildAccountDefaults(t *testing.T) {
	account := buildAccount(store.SIPSettings{Username: "bob", Domain: "pbx.example"})
	for _, want := range []string{":5060", "transport=udp", "regint=300"} {
		if !strings.Contains(account, want) {
			t.Errorf("account missing %q: %s", want, account)
		}
	}
}

func TestBuildAccountRequiresIdentity(t *testing.T) {
	if got := buildAccount(store.SIPSettings{Username: "", Domain: "sip.example.com"}); got != "" {
		t.Errorf("empty username should produce empty account, got %q", got)
	}
	if got := buildAccount(store.SIPSettings{Username: "alice", Domain: ""}); got != "" {
		t.Errorf("empty domain should produce empty account, got %q", got)
	}
}

func TestEscapeParam(t *testing.T) {
	if got := escapeParam("a'b\"c;d@e"); got != "a%27b%22c%3Bd%40e" {
		t.Fatalf("escapeParam = %q", got)
	}
	if got := escapeParam("user.name-1"); got != "user.name-1" {
		t.Fatalf("escapeParam mangled safe characters: %q", got)
	}
}

func TestAccountForLogRedactsPassword(t *testing.T) {
	account := buildAccount(store.SIPSettings{Username: "alice", Password: "hunter2", Domain: "sip.example.com"})
	logged := accountForLog(account)
	if strings.Contains(logged, "hunter2") || strings.Contains(logged, "auth_pass=") {
		t.Fatalf("password leaked into log string: %q", logged)
	}
	if !strings.Contains(logged, "alice") {
		t.Fatalf("identity missing from log string: %q", logged)
	}
	if got := accountForLog("no password here"); got != "no password here" {
		t.Fatalf("plain account mangled: %q", got)
	}
}

func TestEnvSettingsOverrides(t *testing.T) {
	t.Setenv("VOXMAIL_SIP_USERNAME", "envuser")
	t.Setenv("VOXMAIL_SIP_PASSWORD", "envpass")
	t.Setenv("VOXMAIL_SIP_DOMAIN", "env.example")
	t.Setenv("VOXMAIL_SIP_TRANSPORT", "tcp")
	t.Setenv("VOXMAIL_SIP_PORT", "5070")
	t.Setenv("VOXMAIL_SIP_REGINT", "30")
	base := store.SIPSettings{Username: "dbuser", Password: "dbpass", Domain: "db.example", Transport: "udp", Port: 5060, RegInterval: 300}
	out := envSettings(base)
	if out.Username != "envuser" || out.Password != "envpass" || out.Domain != "env.example" {
		t.Fatalf("env overrides not applied: %+v", out)
	}
	if out.Transport != "tcp" || out.Port != 5070 || out.RegInterval != 30 {
		t.Fatalf("numeric env overrides not applied: %+v", out)
	}
}

func TestEnvSettingsIgnoresInvalidPort(t *testing.T) {
	t.Setenv("VOXMAIL_SIP_PORT", "not-a-port")
	base := store.SIPSettings{Port: 5060}
	if out := envSettings(base); out.Port != 5060 {
		t.Fatalf("invalid port replaced the base value: %+v", out)
	}
	t.Setenv("VOXMAIL_SIP_PORT", "70000")
	if out := envSettings(base); out.Port != 5060 {
		t.Fatalf("out-of-range port replaced the base value: %+v", out)
	}
}

func TestEnvSettingsLeavesDefaults(t *testing.T) {
	for _, key := range []string{"VOXMAIL_SIP_USERNAME", "VOXMAIL_SIP_PASSWORD", "VOXMAIL_SIP_DOMAIN", "VOXMAIL_SIP_TRANSPORT", "VOXMAIL_SIP_PORT", "VOXMAIL_SIP_REGINT", "VOXMAIL_SIP_ACCOUNT"} {
		t.Setenv(key, "")
	}
	base := store.SIPSettings{Username: "alice", Password: "pw", Domain: "sip.example.com"}
	if out := envSettings(base); out.Username != "alice" || out.Password != "pw" || out.Domain != "sip.example.com" {
		t.Fatalf("empty env vars clobbered settings: %+v", out)
	}
}
