// Package sip manages the baresip User-Agent lifecycle. It renders the
// baresip config and accounts files from the deployment SIP settings (or the
// VOXMAIL_SIP_ACCOUNT override) and supervises the process so credential or
// port changes from the web console restart baresip automatically. The Go
// bridge connects to baresip over the control socket and reconnects across
// restarts, so process churn is harmless to active configuration.
package sip

import (
	"context"
	"errors"
	"fmt"
	"github.com/voxmail/voxmail/internal/diagnostics"
	"github.com/voxmail/voxmail/internal/operation"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/voxmail/voxmail/internal/maintenance"
	"github.com/voxmail/voxmail/internal/secret"
	"github.com/voxmail/voxmail/internal/store"
)

const (
	DefaultPort                 = 5060
	DefaultTransport            = "udp"
	DefaultRegint               = 300
	defaultLogMaxBytes    int64 = 64 << 20
	maxRespawnAttempts          = 5
	initialRespawnBackoff       = 2 * time.Second
	maxRespawnBackoff           = 30 * time.Second
	stableRespawnUptime         = time.Minute
)

const (
	ProcessDisabled          = "disabled"
	ProcessStarting          = "starting"
	ProcessRunning           = "running"
	ProcessStopping          = "stopping"
	ProcessStopped           = "stopped"
	ProcessDegraded          = "degraded"
	RegistrationDisabled     = "disabled"
	RegistrationUnknown      = "unknown"
	RegistrationRegistering  = "registering"
	RegistrationRegistered   = "registered"
	RegistrationFailed       = "failed"
	RegistrationUnregistered = "unregistered"
)

// Status separates the child-process lifecycle from SIP registration. A
// running baresip process is not proof that its account is registered.
type Status struct {
	Process       string `json:"process"`
	Registration  string `json:"registration"`
	Enabled       bool   `json:"enabled"`
	LastError     string `json:"last_error,omitempty"`
	ErrorCategory string `json:"error_category,omitempty"`
	ResponseCode  int    `json:"response_code,omitempty"`
}

// Baresip supervises a single baresip child process.
type Baresip struct {
	Binary        string
	ConfigDir     string
	ControlSocket string
	AudioDir      string
	LogPath       string
	LogMaxBytes   int64
	MaxCalls      int
	Offline       bool
	Store         *store.Store
	Secrets       *secret.Box
	Log           *slog.Logger

	mu            sync.Mutex
	lifecycle     sync.Mutex
	cmd           *exec.Cmd
	waitDone      chan struct{}
	logHandle     io.Closer
	enabled       bool
	account       string
	stopping      bool
	runCtx        context.Context
	runCancel     context.CancelFunc
	lifetime      context.Context
	process       string
	registration  string
	lastError     string
	errorCategory string
	responseCode  int
	generation    uint64
	crashes       uint8
	restart       restartPolicy
}

// Apply reads the current SIP settings, rewrites the baresip configuration,
// and starts, restarts, or stops baresip as needed. VOXMAIL_SIP_ACCOUNT, when
// set, overrides the database settings for compatibility.
func (b *Baresip) Apply(ctx context.Context) error {
	b.lifecycle.Lock()
	defer b.lifecycle.Unlock()
	return b.apply(ctx)
}

// SetLifetime supplies the application-owned context used for child
// supervision and respawn. HTTP request contexts must never own baresip's
// lifetime; the application calls this once during startup.
func (b *Baresip) SetLifetime(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	b.mu.Lock()
	b.lifetime = ctx
	b.mu.Unlock()
}

func (b *Baresip) supervisorContext(fallback context.Context) context.Context {
	if fallback == nil {
		fallback = context.Background()
	}
	b.mu.Lock()
	ctx := b.lifetime
	b.mu.Unlock()
	if ctx != nil {
		return ctx
	}
	return fallback
}

func (b *Baresip) apply(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	supervisorCtx := b.supervisorContext(ctx)
	b.mu.Lock()
	if b.runCancel != nil {
		b.runCancel()
	}
	b.runCtx, b.runCancel = context.WithCancel(supervisorCtx)
	b.generation++
	b.mu.Unlock()
	settings := b.loadSettings(supervisorCtx)
	settings = envSettings(settings)
	offline := b.Offline
	if value, err := strconv.ParseBool(strings.TrimSpace(os.Getenv("VOXMAIL_OFFLINE_MODE"))); err == nil && value {
		offline = true
	}
	envAccount := strings.TrimSpace(os.Getenv("VOXMAIL_SIP_ACCOUNT"))
	enableCalls := os.Getenv("VOXMAIL_ENABLE_CALLS") == "1"

	account := ""
	if !offline {
		account = envAccount
		if account == "" && settings.Enabled {
			account = buildAccount(settings)
		}
	}
	enabled := !offline && (enableCalls || envAccount != "" || (settings.Enabled && account != ""))
	b.mu.Lock()
	b.stopping = false
	b.account = account
	b.enabled = enabled
	b.lastError = ""
	b.errorCategory = ""
	b.responseCode = 0
	if enabled {
		b.process = ProcessStarting
		b.registration = RegistrationUnknown
	} else {
		b.process = ProcessDisabled
		b.registration = RegistrationDisabled
	}
	b.mu.Unlock()

	if err := b.writeConfig(settings, account != ""); err != nil {
		b.mu.Lock()
		b.process = ProcessDegraded
		b.recordErrorLocked(err)
		b.mu.Unlock()
		return err
	}
	return b.start(supervisorCtx)
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
			settings.LocalPort = port
			settings.RegistrarPort = port
		}
	}
	if value := strings.TrimSpace(os.Getenv("VOXMAIL_SIP_LOCAL_PORT")); value != "" {
		if port, err := strconv.Atoi(value); err == nil && port > 0 && port <= 65535 {
			settings.LocalPort = port
		}
	}
	if value := strings.TrimSpace(os.Getenv("VOXMAIL_SIP_REGISTRAR_PORT")); value != "" {
		if port, err := strconv.Atoi(value); err == nil && port > 0 && port <= 65535 {
			settings.RegistrarPort = port
		}
	}
	if value := strings.TrimSpace(os.Getenv("VOXMAIL_SIP_REGINT")); value != "" {
		if regint, err := strconv.Atoi(value); err == nil && regint >= 0 {
			settings.RegInterval = regint
		}
	}
	if settings.RegistrarPort != 0 {
		/* Keep the pre-split field truthful for callers that still inspect it. */
		settings.Port = settings.RegistrarPort
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
	if stored.Password != "" {
		if b.Secrets == nil {
			stored.Password = ""
			stored.Enabled = false
			if b.Log != nil {
				b.Log.Error("sip: secret box unavailable; disabling stored account")
			}
		} else if plain, err := b.Secrets.Open(stored.Password); err == nil {
			stored.Password = plain
		} else if b.Log != nil {
			b.Log.Error("sip: cannot decrypt stored password", "error", err)
			// Never pass ciphertext to baresip as if it were a password. Keep
			// the stored record intact for recovery, but fail closed by
			// disabling the database-backed account for this apply.
			stored.Password = ""
			stored.Enabled = false
		} else {
			stored.Password = ""
			stored.Enabled = false
		}
	}
	return stored
}

// Stop disables baresip and terminates the current child process.
func (b *Baresip) Stop() {
	b.lifecycle.Lock()
	defer b.lifecycle.Unlock()
	b.mu.Lock()
	b.stopping = true
	b.generation++
	b.enabled = false
	b.process = ProcessStopping
	b.registration = RegistrationDisabled
	if b.runCancel != nil {
		b.runCancel()
		b.runCancel = nil
	}
	b.mu.Unlock()
	_ = b.stop()
	b.mu.Lock()
	if b.cmd == nil {
		b.process = ProcessDisabled
	}
	b.mu.Unlock()
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

// Status returns a snapshot suitable for health/readiness reporting. It never
// infers registration from process existence; only the native registration
// event reporter can move that state to registered.
func (b *Baresip) Status() Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	process := b.process
	registration := b.registration
	if process == "" {
		if b.enabled {
			process = ProcessStopped
		} else {
			process = ProcessDisabled
		}
	}
	if registration == "" {
		if b.enabled {
			registration = RegistrationUnknown
		} else {
			registration = RegistrationDisabled
		}
	}
	return Status{Process: process, Registration: registration, Enabled: b.enabled, LastError: b.lastError, ErrorCategory: b.errorCategory, ResponseCode: b.responseCode}
}

// SetRegistrationState is called by the Go bridge when the pinned native shim
// observes a Baresip registration event. Unknown phases are ignored so a
// malformed bridge event cannot make readiness look healthy.
func (b *Baresip) SetRegistrationState(phase, reason string) {
	state := map[string]string{
		"registering":  RegistrationRegistering,
		"registered":   RegistrationRegistered,
		"failed":       RegistrationFailed,
		"unregistered": RegistrationUnregistered,
	}[strings.ToLower(strings.TrimSpace(phase))]
	if state == "" {
		return
	}
	b.mu.Lock()
	if b.enabled {
		b.registration = state
		if state == RegistrationFailed {
			b.lastError = strings.TrimSpace(reason)
			b.errorCategory, b.responseCode = registrationFailure(reason)
		} else {
			b.lastError = ""
			b.errorCategory = ""
			b.responseCode = 0
		}
	}
	b.mu.Unlock()
}

// rtpPortBase is the low end of the UDP range baresip binds RTP media on.
const rtpPortBase = 10000

// rtpPortRange sizes the published RTP range from the call limit. Each active
// call binds an RTP/RTCP port pair and teardown may rebind briefly, so four
// ports per call with a 32-port floor covers the real need. The old fixed
// 10000-10100 published 101 ports whatever the call limit was, an
// unnecessarily wide listening surface for small deployments.
func rtpPortRange(maxCalls int) (int, int) {
	if maxCalls <= 0 {
		maxCalls = 10
	}
	ports := maxCalls * 4
	if ports < 32 {
		ports = 32
	}
	return rtpPortBase, rtpPortBase + ports - 1
}

func (b *Baresip) writeConfig(settings store.SIPSettings, hasAccount bool) error {
	if b.ConfigDir == "" {
		return fmt.Errorf("sip: config dir is required")
	}
	if err := os.MkdirAll(b.ConfigDir, 0700); err != nil {
		return err
	}
	localPort := settings.LocalPort
	if localPort == 0 {
		localPort = settings.Port
	}
	if localPort == 0 {
		localPort = DefaultPort
	}
	transport := strings.ToLower(strings.TrimSpace(settings.Transport))
	if transport == "" {
		transport = DefaultTransport
	}
	maxCalls := b.MaxCalls
	if maxCalls <= 0 {
		maxCalls = 10
	}
	rtpLow, rtpHigh := rtpPortRange(maxCalls)
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
ausrc_srate 8000
auplay_srate 8000
ausrc_channels 1
auplay_channels 1
ausrc_format s16
auplay_format s16
audio_buffer 60-200
rtp_ports %d-%d
call_max_calls %d
sip_listen 0.0.0.0:%d
sip_transports %s
`, rtpLow, rtpHigh, maxCalls, localPort, transport)
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

// start stops any existing process without holding b.mu while waiting for its
// exit, then starts the configured child. Waiting under b.mu deadlocks with
// onExit, which must acquire the same mutex after cmd.Wait returns.
func (b *Baresip) start(ctx context.Context) error {
	if err := b.stop(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.startLocked(ctx)
}

// startLocked starts a child and requires b.mu. It is retained as a narrow
// low-level helper for tests and the respawn path; it never waits for an old
// child to exit.
func (b *Baresip) startLocked(ctx context.Context) error {
	if !b.enabled {
		b.process = ProcessDisabled
		return nil
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			b.process = ProcessDegraded
			b.recordErrorLocked(err)
			return err
		}
	}
	binary := b.Binary
	if binary == "" {
		binary = "baresip"
	}
	var logFile io.Writer = os.Stdout
	var logHandle io.Closer
	if b.LogPath != "" {
		maxBytes := b.LogMaxBytes
		if maxBytes <= 0 {
			maxBytes = defaultLogMaxBytes
		}
		handle, err := newBoundedLog(b.LogPath, maxBytes)
		if err != nil {
			b.process = ProcessDegraded
			b.recordErrorLocked(err)
			return err
		}
		logFile = handle
		logHandle = handle
	}
	// Baresip is owned by this supervisor, not by the HTTP request that
	// triggered Apply. Its lifetime is controlled by Stop and respawn.
	cmd := exec.Command(binary, diagnostics.ProcessArguments("baresip", []string{"-f", b.ConfigDir})...)
	safeLog := diagnostics.NewOutput(ctx, "baresip", logFile)
	cmd.Stdout = safeLog
	cmd.Stderr = safeLog
	cmd.Env = append(os.Environ(),
		"VOXMAIL_CONTROL_SOCKET="+b.ControlSocket,
		"VOXMAIL_AUDIO_DIR="+b.AudioDir,
		"VOXMAIL_VERBOSE="+strconv.FormatBool(diagnostics.Enabled()),
	)
	// The native child creates call paths before publishing their events.
	// Protect its tree before exec, including the graceful teardown window.
	releaseAudio := maintenance.Protect(b.AudioDir)
	if err := cmd.Start(); err != nil {
		releaseAudio()
		if logHandle != nil {
			_ = logHandle.Close()
		}
		err = operation.Wrap("baresip", "start", operation.Failed, err)
		b.process = ProcessDegraded
		b.recordErrorLocked(err)
		return err
	}
	b.cmd = cmd
	b.process = ProcessRunning
	b.lastError = ""
	b.errorCategory = ""
	b.responseCode = 0
	b.logHandle = logHandle
	waitDone := make(chan struct{})
	b.waitDone = waitDone
	generation := b.generation
	runCtx := b.runCtx
	if runCtx == nil {
		runCtx = ctx
	}
	if runCtx == nil {
		runCtx = context.Background()
	}
	if b.Log != nil {
		b.Log.Info("baresip started", "dir", b.ConfigDir, "account", accountForLog(b.account))
	}
	go b.resetAfterStableUptime(cmd, waitDone, runCtx, generation, b.restart.policy().stable)
	go func() {
		err := cmd.Wait()
		_ = safeLog.Flush()
		releaseAudio()
		b.onExit(cmd, waitDone, runCtx, generation, err)
	}()
	return nil
}

func (b *Baresip) onExit(cmd *exec.Cmd, waitDone chan struct{}, runCtx context.Context,
	generation uint64, exitErr error) {
	b.mu.Lock()
	current := b.cmd == cmd
	account := accountForLog(b.account)
	shouldRespawn := current && !b.stopping && b.enabled &&
		generation == b.generation && runCtx.Err() == nil
	if current {
		b.cmd = nil
		if shouldRespawn {
			b.recordCrashLocked()
			b.process = ProcessDegraded
			b.registration = RegistrationUnknown
			b.lastError = "baresip exited unexpectedly"
			b.errorCategory = string(operation.ProcessExit)
			b.responseCode = 0
			if exitErr != nil {
				b.recordErrorLocked(operation.Wrap("baresip", "wait", operation.ProcessExit, exitErr))
			}
		} else if b.enabled {
			b.process = ProcessStopped
			b.registration = RegistrationUnknown
		} else {
			b.process = ProcessDisabled
			b.registration = RegistrationDisabled
		}
	}
	if current {
		if b.logHandle != nil {
			_ = b.logHandle.Close()
			b.logHandle = nil
		}
		b.waitDone = nil
	}
	b.mu.Unlock()
	// Stop waits for this signal. Publish it only after the lifecycle state and
	// current-child pointer have been committed, so a caller cannot observe
	// ProcessStopping with a child that has already exited.
	close(waitDone)
	if b.Log != nil && current && shouldRespawn {
		b.Log.Warn("baresip exited; restarting", "account", account)
	} else if b.Log != nil && current {
		b.Log.Info("baresip exited", "account", account)
	}
	if shouldRespawn {
		b.respawn(runCtx, generation)
	}
}

func (b *Baresip) respawn(runCtx context.Context, generation uint64) {
	for attempt := 1; attempt <= maxRespawnAttempts; attempt++ {
		b.mu.Lock()
		if generation != b.generation || b.cmd != nil || b.stopping || !b.enabled {
			b.mu.Unlock()
			return
		}
		backoff := b.restart.policy().delay(b.crashes)
		b.mu.Unlock()
		if b.Log != nil {
			b.Log.Warn("waiting to restart baresip", "delay", backoff, "attempt", attempt)
		}
		timer := time.NewTimer(backoff)
		select {
		case <-runCtx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		b.lifecycle.Lock()
		b.mu.Lock()
		if generation != b.generation || b.cmd != nil || b.stopping || !b.enabled || runCtx.Err() != nil {
			b.mu.Unlock()
			b.lifecycle.Unlock()
			return
		}
		err := b.startLocked(runCtx)
		if err != nil && runCtx.Err() == nil {
			b.recordCrashLocked()
		}
		b.mu.Unlock()
		b.lifecycle.Unlock()
		if err == nil {
			return
		}
		if runCtx.Err() != nil {
			return
		}
		if b.Log != nil {
			b.Log.Error("baresip restart failed", "attempt", attempt, "max_attempts", maxRespawnAttempts, "error", err)
		}
	}
	if b.Log != nil {
		b.Log.Error("baresip restart abandoned after repeated failures", "attempts", maxRespawnAttempts)
	}
	b.mu.Lock()
	if generation == b.generation && b.cmd == nil && b.enabled && !b.stopping {
		b.process = ProcessDegraded
	}
	b.mu.Unlock()
}

// stop terminates the current child and waits without holding b.mu. The
// supervisor callback acquires b.mu when it observes the child exit.
func (b *Baresip) stop() error {
	b.mu.Lock()
	cmd := b.cmd
	waitDone := b.waitDone
	controlSocket := b.ControlSocket
	b.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return removeDeadControlSocket(controlSocket)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	select {
	case <-waitDone:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		if waitDone != nil {
			<-waitDone
		}
	}
	return removeDeadControlSocket(controlSocket)
}

// removeDeadControlSocket completes the supervisor's cleanup after Baresip
// exits. The pinned Baresip executable can leave a dead AF_UNIX pathname after
// SIGTERM even though the module's normal close hook handles its resources.
// Never unlink a path that is not a socket or is still serving connections.
func removeDeadControlSocket(path string) error {
	if path == "" {
		return nil
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat baresip control socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("baresip control path is not a Unix socket: %s", path)
	}
	conn, err := net.DialTimeout("unix", path, 100*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		return fmt.Errorf("baresip control socket is still accepting connections: %s", path)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) && !errors.Is(err, syscall.ENOENT) {
		return fmt.Errorf("probe baresip control socket: %w", err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale baresip control socket: %w", err)
	}
	return nil
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
	domain = formatSIPHost(domain)
	registrarPort := settings.RegistrarPort
	if registrarPort == 0 {
		registrarPort = settings.Port
	}
	if registrarPort == 0 {
		registrarPort = DefaultPort
	}
	transport := strings.ToLower(strings.TrimSpace(settings.Transport))
	if transport == "" {
		transport = DefaultTransport
	}
	regint := settings.RegInterval
	if regint <= 0 {
		regint = DefaultRegint
	}
	uriUser := escapeParam(username)
	return fmt.Sprintf("<sip:%s@%s:%d;transport=%s>;auth_user=%s;auth_pass=%s;regint=%d",
		uriUser, domain, registrarPort, transport, accountParam(username), accountParam(settings.Password), regint)
}

func formatSIPHost(host string) string {
	host = strings.TrimSpace(host)
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		return host
	}
	if ip := net.ParseIP(host); ip != nil && strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}

// escapeParam percent-encodes a value used in the SIP URI user component.
// Baresip decodes this URI component according to SIP URI rules.
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

// accountParam renders a Baresip account-file parameter. auth_user and
// auth_pass are not URI components: Baresip's pinned parameter parser does
// not percent-decode them. Keep the compact form for ordinary credentials,
// and quote values containing separators or whitespace so the exact bytes
// reach the digest-auth implementation.
func accountParam(value string) string {
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '.' || c == '_' || c == '~' || c == '+' {
			continue
		}
		return `"` + value + `"`
	}
	return value
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

// recordErrorLocked stores typed process/storage facts; callers own b.mu.
func (b *Baresip) recordErrorLocked(err error) {
	b.lastError = err.Error()
	b.errorCategory = string(operation.Classify(err))
	b.responseCode = 0
}

// The native bridge supplies a response code as the leading token. Arbitrary
// provider prose is retained only for restricted logs, never for classification.
func registrationFailure(reason string) (string, int) {
	category, code := operation.Protocol, 0
	fields := strings.Fields(reason)
	if len(fields) > 0 && len(fields[0]) == 3 {
		parsed, err := strconv.Atoi(fields[0])
		if err == nil && parsed >= 300 && parsed <= 699 {
			code = parsed
		}
	}
	switch code {
	case 401, 403, 407:
		category = operation.Authentication
	case 408, 504:
		category = operation.Timeout
	case 404:
		category = operation.SIPAccount
	}
	return string(category), code
}
