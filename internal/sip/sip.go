// Package sip manages the baresip User-Agent lifecycle. It renders the
// baresip config and accounts files from the deployment SIP settings (or the
// VOXMAIL_SIP_ACCOUNT override) and supervises the process so credential or
// port changes from the web console restart baresip automatically. The Go
// bridge connects to baresip over the control socket and reconnects across
// restarts, so process churn is harmless to active configuration.
package sip

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/voxmail/voxmail/internal/secret"
	"github.com/voxmail/voxmail/internal/store"
)

const (
	DefaultPort      = 5060
	DefaultTransport = "udp"
	DefaultRegint    = 300
)

// Baresip supervises a single baresip child process.
type Baresip struct {
	Binary        string
	ConfigDir     string
	ControlSocket string
	AudioDir      string
	LogPath       string
	MaxCalls      int
	Store         *store.Store
	Secrets       *secret.Box
	Log           *slog.Logger

	mu        sync.Mutex
	cmd       *exec.Cmd
	waitDone  chan struct{}
	logHandle *os.File
	enabled   bool
	account   string
	stopping  bool
}

// Apply reads the current SIP settings, rewrites the baresip configuration,
// and starts, restarts, or stops baresip as needed. VOXMAIL_SIP_ACCOUNT, when
// set, overrides the database settings for compatibility.
func (b *Baresip) Apply(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stopping = false
	return b.applyLocked(ctx)
}

func (b *Baresip) applyLocked(ctx context.Context) error {
	settings := b.loadSettings(ctx)
	settings = envSettings(settings)
	envAccount := strings.TrimSpace(os.Getenv("VOXMAIL_SIP_ACCOUNT"))
	enableCalls := os.Getenv("VOXMAIL_ENABLE_CALLS") == "1"

	account := envAccount
	if account == "" && settings.Enabled {
		account = buildAccount(settings)
	}
	enabled := enableCalls || envAccount != "" || (settings.Enabled && account != "")
	b.account = account
	b.enabled = enabled

	if err := b.writeConfig(settings, account != ""); err != nil {
		return err
	}
	return b.startLocked(ctx)
}

// envSettings lets deployment overlays supply SIP credentials without touching
// the database. VOXMAIL_SIP_ACCOUNT remains supported as a ready-made account
// line, while the individual variables map onto the normal settings fields.
func envSettings(settings store.SIPSettings) store.SIPSettings {
	if value := strings.TrimSpace(os.Getenv("VOXMAIL_SIP_USERNAME")); value != "" {
		settings.Username = value
	}
	if value := os.Getenv("VOXMAIL_SIP_PASSWORD"); value != "" {
		settings.Password = value
	}
	if value := strings.TrimSpace(os.Getenv("VOXMAIL_SIP_DOMAIN")); value != "" {
		settings.Domain = value
	}
	if value := strings.TrimSpace(os.Getenv("VOXMAIL_SIP_TRANSPORT")); value != "" {
		settings.Transport = value
	}
	if value := strings.TrimSpace(os.Getenv("VOXMAIL_SIP_PORT")); value != "" {
		if port, err := strconv.Atoi(value); err == nil && port > 0 && port <= 65535 {
			settings.Port = port
		}
	}
	if value := strings.TrimSpace(os.Getenv("VOXMAIL_SIP_REGINT")); value != "" {
		if regint, err := strconv.Atoi(value); err == nil && regint >= 0 {
			settings.RegInterval = regint
		}
	}
	return settings
}

// accountForLog redacts the auth_pass component so credentials never reach
// logs while still showing which identity the UAC registered as.
func accountForLog(account string) string {
	if at := strings.Index(account, ";auth_pass="); at >= 0 {
		account = account[:at]
	}
	return account
}

func (b *Baresip) loadSettings(ctx context.Context) store.SIPSettings {
	settings := store.SIPSettings{Port: DefaultPort, Transport: DefaultTransport, RegInterval: DefaultRegint}
	if b.Store == nil {
		return settings
	}
	stored, err := b.Store.GetSIP(ctx)
	if err != nil {
		return settings
	}
	if stored.Password != "" && b.Secrets != nil {
		if plain, err := b.Secrets.Open(stored.Password); err == nil {
			stored.Password = plain
		} else if b.Log != nil {
			b.Log.Error("sip: cannot decrypt stored password", "error", err)
		}
	}
	return stored
}

// Stop disables baresip and terminates the current child process.
func (b *Baresip) Stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stopping = true
	b.enabled = false
	b.stopLocked()
}

// AccountLine returns the rendered accounts entry for the current settings,
// or the environment override when one is configured.
func (b *Baresip) AccountLine() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.account
}

// Enabled reports whether baresip is currently meant to run.
func (b *Baresip) Enabled() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.enabled
}

func (b *Baresip) writeConfig(settings store.SIPSettings, hasAccount bool) error {
	if b.ConfigDir == "" {
		return fmt.Errorf("sip: config dir is required")
	}
	if err := os.MkdirAll(b.ConfigDir, 0700); err != nil {
		return err
	}
	port := settings.Port
	if port == 0 {
		port = DefaultPort
	}
	transport := strings.ToLower(strings.TrimSpace(settings.Transport))
	if transport == "" {
		transport = DefaultTransport
	}
	maxCalls := b.MaxCalls
	if maxCalls <= 0 {
		maxCalls = 10
	}
	config := fmt.Sprintf(`module_path /usr/local/lib/baresip/modules
module_app voxmail.so
module account.so
module g711.so
module auconv.so
module auresamp.so
module aubridge.so
module aufile.so
module in_band_dtmf.so
module ice.so
module stun.so
module srtp.so
module dtls_srtp.so
module stdio.so
call_accept yes
poll_method poll
audio_source voxmail,
audio_player voxmail,
audio_alert voxmail,
audio_buffer 60-200
rtp_ports 10000-10100
call_max_calls %d
sip_listen 0.0.0.0:%d
sip_transports %s
`, maxCalls, port, transport)
	if err := writeFileAtomic(b.ConfigDir, "config", config); err != nil {
		return err
	}
	if hasAccount {
		if err := writeFileAtomic(b.ConfigDir, "accounts", b.account+"\n"); err != nil {
			return err
		}
	} else {
		_ = os.Remove(filepath.Join(b.ConfigDir, "accounts"))
	}
	return nil
}

func (b *Baresip) startLocked(ctx context.Context) error {
	b.stopLocked()
	if !b.enabled {
		return nil
	}
	binary := b.Binary
	if binary == "" {
		binary = "baresip"
	}
	logFile := os.Stdout
	if b.LogPath != "" {
		if err := os.MkdirAll(filepath.Dir(b.LogPath), 0700); err != nil {
			return err
		}
		handle, err := os.OpenFile(b.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			return err
		}
		logFile = handle
	}
	cmd := exec.CommandContext(ctx, binary, "-f", b.ConfigDir)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Env = append(os.Environ(),
		"VOXMAIL_CONTROL_SOCKET="+b.ControlSocket,
		"VOXMAIL_AUDIO_DIR="+b.AudioDir,
	)
	if err := cmd.Start(); err != nil {
		if logFile != os.Stdout {
			_ = logFile.Close()
		}
		return fmt.Errorf("start baresip: %w", err)
	}
	b.cmd = cmd
	if logFile != os.Stdout {
		b.logHandle = logFile
	}
	waitDone := make(chan struct{})
	b.waitDone = waitDone
	if b.Log != nil {
		b.Log.Info("baresip started", "dir", b.ConfigDir, "account", accountForLog(b.account))
	}
	go func() {
		_ = cmd.Wait()
		close(waitDone)
		b.onExit(cmd)
	}()
	return nil
}

func (b *Baresip) onExit(cmd *exec.Cmd) {
	b.mu.Lock()
	current := b.cmd == cmd
	if current {
		b.cmd = nil
	}
	if current && b.logHandle != nil {
		_ = b.logHandle.Close()
		b.logHandle = nil
		b.waitDone = nil
	}
	b.mu.Unlock()
	if b.Log != nil && current {
		b.Log.Warn("baresip exited; restarting", "account", accountForLog(b.account))
	}
	if current {
		b.respawn()
	}
}

func (b *Baresip) respawn() {
	time.Sleep(2 * time.Second)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cmd != nil || b.stopping || !b.enabled {
		return
	}
	_ = b.startLocked(context.Background())
}

func (b *Baresip) stopLocked() {
	cmd := b.cmd
	if cmd == nil || cmd.Process == nil {
		b.cmd = nil
		return
	}
	b.cmd = nil
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-b.waitDone:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		if b.waitDone != nil {
			<-b.waitDone
		}
	}
	if b.logHandle != nil {
		_ = b.logHandle.Close()
		b.logHandle = nil
	}
	b.waitDone = nil
	if b.Log != nil {
		b.Log.Info("baresip stopped")
	}
}

// buildAccount renders a baresip accounts line from the database settings.
// The password travels in auth_pass rather than in the SIP URI so it never
// leaks into logs that print the account identity.
func buildAccount(settings store.SIPSettings) string {
	domain := strings.TrimSpace(settings.Domain)
	username := strings.TrimSpace(settings.Username)
	if domain == "" || username == "" {
		return ""
	}
	port := settings.Port
	if port == 0 {
		port = DefaultPort
	}
	transport := strings.ToLower(strings.TrimSpace(settings.Transport))
	if transport == "" {
		transport = DefaultTransport
	}
	regint := settings.RegInterval
	if regint <= 0 {
		regint = DefaultRegint
	}
	user := escapeParam(username)
	password := escapeParam(settings.Password)
	return fmt.Sprintf("<sip:%s@%s:%d;transport=%s>;auth_user=%s;auth_pass=%s;regint=%d",
		user, domain, port, transport, user, password, regint)
}

// escapeParam percent-encodes a value used inside the accounts file so SIP
// separators and control characters cannot break the account line.
func escapeParam(value string) string {
	const hex = "0123456789ABCDEF"
	var out strings.Builder
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '.' || c == '_' || c == '~' {
			out.WriteByte(c)
			continue
		}
		out.WriteByte('%')
		out.WriteByte(hex[c>>4])
		out.WriteByte(hex[c&0x0f])
	}
	return out.String()
}

func writeFileAtomic(dir, name, content string) error {
	tmp, err := os.CreateTemp(dir, "."+name+"-*")
	if err != nil {
		return err
	}
	path := tmp.Name()
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		_ = os.Remove(path)
		return err
	}
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(path)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	return os.Rename(path, filepath.Join(dir, name))
}
