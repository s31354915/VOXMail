package sip

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/voxmail/voxmail/internal/secret"
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

func TestBaresipStatusSeparatesProcessAndRegistration(t *testing.T) {
	b := &Baresip{enabled: true, process: ProcessRunning, registration: RegistrationUnknown}
	status := b.Status()
	if status.Process != ProcessRunning || status.Registration != RegistrationUnknown {
		t.Fatalf("initial status=%+v, want running/unknown", status)
	}
	b.SetRegistrationState("failed", "401 Unauthorized")
	status = b.Status()
	if status.Registration != RegistrationFailed || status.LastError != "401 Unauthorized" {
		t.Fatalf("failed registration status=%+v", status)
	}
	b.SetRegistrationState("registered", "")
	status = b.Status()
	if status.Process != ProcessRunning || status.Registration != RegistrationRegistered || status.LastError != "" {
		t.Fatalf("registered status=%+v", status)
	}
	b.SetRegistrationState("not-a-real-phase", "should be ignored")
	if status = b.Status(); status.Registration != RegistrationRegistered {
		t.Fatalf("unknown phase changed status=%+v", status)
	}
}

func TestBaresipStopWithoutChildReportsDisabled(t *testing.T) {
	b := &Baresip{enabled: true, process: ProcessRunning, registration: RegistrationRegistered}
	b.Stop()
	status := b.Status()
	if status.Enabled || status.Process != ProcessDisabled || status.Registration != RegistrationDisabled {
		t.Fatalf("stopped status=%+v", status)
	}
}

func TestBaresipLifetimeCancellationStopsRespawnBackoff(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "fake-baresip-exit.sh")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nsleep 0.05\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := &Baresip{Binary: binary, ConfigDir: filepath.Join(dir, "config"), ControlSocket: filepath.Join(dir, "baresip.sock"), enabled: true, runCtx: ctx}
	b.mu.Lock()
	if err := b.startLocked(ctx); err != nil {
		b.mu.Unlock()
		t.Fatalf("startLocked: %v", err)
	}
	b.mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		degraded := b.process == ProcessDegraded && b.cmd == nil
		b.mu.Unlock()
		if degraded {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	time.Sleep(initialRespawnBackoff + 150*time.Millisecond)
	b.mu.Lock()
	degraded := b.process == ProcessDegraded && b.cmd == nil
	b.mu.Unlock()
	if !degraded {
		b.Stop()
		t.Fatal("baresip respawned after application lifetime cancellation")
	}
	b.Stop()
}

func TestBaresipRapidApplyStopLeavesNoChild(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "fake-baresip-sleep.sh")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nsleep 0.2\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VOXMAIL_ENABLE_CALLS", "1")
	b := &Baresip{Binary: binary, ConfigDir: filepath.Join(dir, "config"), ControlSocket: filepath.Join(dir, "baresip.sock")}
	for i := 0; i < 3; i++ {
		if err := b.Apply(context.Background()); err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
	}
	b.Stop()
	status := b.Status()
	if status.Enabled || status.Process != ProcessDisabled {
		t.Fatalf("rapid apply/stop status=%+v", status)
	}
}

func TestBaresipStopRemovesDeadControlSocket(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "fake-baresip-sleep.sh")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nsleep 5\n"), 0700); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(dir, "baresip.sock")
	b := &Baresip{Binary: binary, ConfigDir: filepath.Join(dir, "config"), ControlSocket: socketPath, enabled: true}
	b.mu.Lock()
	if err := b.startLocked(context.Background()); err != nil {
		b.mu.Unlock()
		t.Fatalf("startLocked: %v", err)
	}
	b.mu.Unlock()
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		b.Stop()
		t.Fatalf("listen: %v", err)
	}
	if err := listener.Close(); err != nil {
		b.Stop()
		t.Fatalf("close listener: %v", err)
	}
	b.Stop()
	if _, err := os.Lstat(socketPath); !os.IsNotExist(err) {
		t.Fatalf("control socket after Stop: err=%v, want not exist", err)
	}
}

func TestRemoveDeadControlSocketPreservesLiveListener(t *testing.T) {
	path := filepath.Join(t.TempDir(), "baresip.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	if err := removeDeadControlSocket(path); err == nil {
		t.Fatal("removeDeadControlSocket accepted a live listener")
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("live control socket was removed: %v", err)
	}
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

func TestBuildAccountPreservesReservedAuthenticationValues(t *testing.T) {
	password := `p%;\" qé`
	account := buildAccount(store.SIPSettings{
		Username: "alice+tag",
		Password: password,
		Domain:   "sip.example.com",
	})
	if !strings.Contains(account, "<sip:alice%2Btag@sip.example.com") {
		t.Fatalf("SIP URI username was not encoded: %q", account)
	}
	if !strings.Contains(account, `auth_user=alice+tag`) {
		t.Fatalf("auth username was not kept as an account parameter: %q", account)
	}
	if !strings.Contains(account, `auth_pass="p%;\" qé"`) {
		t.Fatalf("reserved password was not quoted exactly: %q", account)
	}
}

func TestSIPPortsAreIndependent(t *testing.T) {
	settings := store.SIPSettings{
		Username:      "alice",
		Password:      "s3cret",
		Domain:        "sip.example.com",
		LocalPort:     5080,
		RegistrarPort: 5070,
		Transport:     "tcp",
		RegInterval:   120,
	}
	account := buildAccount(settings)
	if !strings.Contains(account, "sip.example.com:5070;transport=tcp") {
		t.Fatalf("account did not use registrar port: %s", account)
	}
	dir := t.TempDir()
	b := &Baresip{ConfigDir: dir, MaxCalls: 1}
	if err := b.writeConfig(settings, true); err != nil {
		t.Fatalf("writeConfig: %v", err)
	}
	config, err := os.ReadFile(filepath.Join(dir, "config"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), "sip_listen 0.0.0.0:5080") {
		t.Fatalf("config did not use local port: %s", config)
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

func TestSIPZeroRegistrationIntervalUsesDefaultEverywhere(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	box, err := secret.New("test-key-with-more-than-32-characters-123456")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertSIP(context.Background(), box, store.SIPSettings{Domain: "sip.example.com", Username: "bob", RegInterval: 0}); err != nil {
		t.Fatal(err)
	}
	settings, err := db.GetSIP(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if settings.RegInterval != DefaultRegint || !strings.Contains(buildAccount(settings), "regint=300") {
		t.Fatalf("zero interval was not normalized: settings=%+v account=%q", settings, buildAccount(settings))
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

func TestWriteConfigPinsNativeMediaContract(t *testing.T) {
	dir := t.TempDir()
	b := &Baresip{ConfigDir: dir, MaxCalls: 1}
	if err := b.writeConfig(store.SIPSettings{}, false); err != nil {
		t.Fatalf("writeConfig: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "config"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	config := string(data)
	for _, want := range []string{
		"ausrc_srate 8000",
		"auplay_srate 8000",
		"ausrc_channels 1",
		"auplay_channels 1",
		"ausrc_format s16",
		"auplay_format s16",
	} {
		if !strings.Contains(config, want) {
			t.Errorf("config missing %q:\n%s", want, config)
		}
	}
}

func TestLoadSettingsDisablesSIPOnPasswordDecryptionFailure(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	box, err := secret.New("test-key-with-more-than-32-characters-123456")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertSIP(context.Background(), box, store.SIPSettings{
		Domain: "sip.example.com", Username: "alice", Password: "secret", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`UPDATE sip_settings SET password='not-valid-ciphertext' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	loaded := (&Baresip{Store: db, Secrets: box}).loadSettings(context.Background())
	if loaded.Enabled || loaded.Password != "" {
		t.Fatalf("invalid secret was not failed closed: %+v", loaded)
	}
}

func TestEscapeParam(t *testing.T) {
	if got := escapeParam("a'b\"c;d@e"); got != "a%27b%22c%3Bd%40e" {
		t.Fatalf("escapeParam = %q", got)
	}
	if got := escapeParam("user.name-1"); got != "user.name-1" {
		t.Fatalf("escapeParam mangled safe characters: %q", got)
	}
	if got := escapeParam("p%;\" qé"); got != "p%25%3B%22%20q%C3%A9" {
		t.Fatalf("escapeParam did not encode reserved/non-ASCII bytes: %q", got)
	}
}

func TestBuildAccountFormatsIPv6Registrar(t *testing.T) {
	account := buildAccount(store.SIPSettings{
		Username:      "alice",
		Password:      "secret",
		Domain:        "2001:db8::10",
		RegistrarPort: 5061,
		Transport:     "tls",
	})
	if !strings.Contains(account, "<sip:alice@[2001:db8::10]:5061;transport=tls>") {
		t.Fatalf("IPv6 registrar was not bracketed: %q", account)
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

func TestAccountParamOnlyQuotesValuesThatNeedIt(t *testing.T) {
	if got := accountParam("s3cret"); got != "s3cret" {
		t.Fatalf("ordinary value was unexpectedly quoted: %q", got)
	}
	if got := accountParam("alice+tag"); got != "alice+tag" {
		t.Fatalf("SIP auth username was unexpectedly quoted: %q", got)
	}
	if got := accountParam(`p%;\" qé`); got != `"p%;\" qé"` {
		t.Fatalf("reserved value was not quoted without rewriting: %q", got)
	}
}

func TestBaresipRotatesOversizedLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "baresip.log")
	if err := os.WriteFile(path, []byte("old log"), 0600); err != nil {
		t.Fatal(err)
	}
	b := &Baresip{LogPath: path, LogMaxBytes: 4}
	if err := b.rotateLogIfNeeded(); err != nil {
		t.Fatal(err)
	}
	rotated, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if string(rotated) != "old log" {
		t.Fatalf("rotated log=%q, want old log", rotated)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("original log still exists: %v", err)
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
