package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/voxmail/voxmail/internal/diagnostics"
	"github.com/voxmail/voxmail/internal/folderpolicy"
	"github.com/voxmail/voxmail/internal/imapcheck"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/voxmail/voxmail/internal/auth"
	"github.com/voxmail/voxmail/internal/mailer"
	"github.com/voxmail/voxmail/internal/secret"
	"github.com/voxmail/voxmail/internal/speech"
	"github.com/voxmail/voxmail/internal/store"
)

//go:embed static/index.html static/console.bundle.js console/console.css console/console-*.css
var consolePageFiles embed.FS

var indexHTML = assembledConsole()

// themeNameRe bounds VOXMAIL_THEME values to safe embedded-file name parts.
var themeNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// consoleStylesPath resolves the console stylesheet for the configured theme.
// Themes are drop-in stylesheets named console-<theme>.css next to the default
// console.css; VOXMAIL_THEME=<theme> serves one, anything unset, malformed or
// missing falls back to the default stylesheet.
func consoleStylesPath() string {
	name := strings.ToLower(strings.TrimSpace(os.Getenv("VOXMAIL_THEME")))
	if name == "" {
		return "console/console.css"
	}
	if !themeNameRe.MatchString(name) {
		slog.Warn("ignoring VOXMAIL_THEME: invalid theme name", "theme", name)
		return "console/console.css"
	}
	styles := "console/console-" + name + ".css"
	if _, err := consolePageFiles.ReadFile(styles); err != nil {
		slog.Warn("ignoring VOXMAIL_THEME: theme stylesheet not embedded", "theme", name)
		return "console/console.css"
	}
	return styles
}

func assembledConsole() []byte {
	read := func(path string) string {
		data, err := consolePageFiles.ReadFile(path)
		if err != nil {
			panic(err)
		}
		return string(data)
	}
	html := read("static/index.html")
	for marker, path := range map[string]string{
		"{{CONSOLE_SCRIPT}}": "static/console.bundle.js",
		"{{CONSOLE_STYLES}}": consoleStylesPath(),
	} {
		if strings.Count(html, marker) != 1 {
			panic("console template must contain exactly one " + marker)
		}
		html = strings.Replace(html, marker, read(path), 1)
	}
	return []byte(html)
}

//go:embed static/assets/*
var consoleAssets embed.FS

var contentSecurityPolicy = fmt.Sprintf("default-src 'none'; script-src 'sha256-%s'; style-src 'sha256-%s'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; frame-src 'none'; media-src 'self' blob:; object-src 'none'", embeddedAssetHash("<script", "</script>"), embeddedAssetHash("<style", "</style>"))

func embeddedAssetHash(open, close string) string {
	html := string(indexHTML)
	start := strings.Index(html, open)
	if start < 0 {
		return ""
	}
	tagEnd := strings.Index(html[start:], ">")
	if tagEnd < 0 {
		return ""
	}
	start += tagEnd + 1
	end := strings.Index(html[start:], close)
	if end < 0 {
		return ""
	}
	digest := sha256.Sum256([]byte(html[start : start+end]))
	return base64.StdEncoding.EncodeToString(digest[:])
}

// SIPController applies deployment SIP changes to the local baresip process.
// It is optional: without it the console still persists SIP settings, but no
// baresip child is managed.
type SIPController interface {
	Apply(context.Context) error
}

// SIPApplyGate lets the production call service reject a disruptive restart
// while a call or pending alert is active. It is optional for small test
// deployments that do not run the call bridge.
type SIPApplyGate interface {
	BeginSIPApply(context.Context) (func(), error)
}

// RuntimeStatus is the public health snapshot. The fields deliberately remain
// separate: a running process, a connected bridge, a registered SIP account,
// and usable media are different facts.
type RuntimeStatus struct {
	PromptState   string `json:"prompt_state,omitempty"`
	PromptVoice   string `json:"prompt_voice,omitempty"`
	PromptError   string `json:"prompt_error,omitempty"`
	Process       string `json:"process,omitempty"`
	Bridge        string `json:"bridge,omitempty"`
	Registration  string `json:"registration,omitempty"`
	Media         string `json:"media,omitempty"`
	LastError     string `json:"-"`
	ErrorCategory string `json:"-"`
	ResponseCode  int    `json:"-"`
}

// AlertService places outgoing test alert calls on demand. It is optional:
// without it the console reports that the call service is unavailable.
type AlertService interface {
	TestAlert(context.Context, string) error
}

// VoiceActivator warms an installed voice and refreshes the live
// static prompt set before the voice preference is persisted.
type VoiceActivator interface {
	ActivateVoice(context.Context, string, int, int) error
}

type VoiceInstaller interface {
	Install(context.Context, string, speech.VoiceSpec) error
}

// VoicePreviewer synthesizes a short, bounded sample for an already selected
// voice. It is intentionally separate from VoiceActivator so tests and
// deployments that do not run Piper can omit preview support.
type VoicePreviewer interface {
	PreviewVoice(context.Context, string, int) ([]byte, error)
}

// VoiceWarm controls the per-model TTS engine pool: administrator keep-warm
// pinning and the live display of which resident models are loaded.
type VoiceWarm interface {
	KeepWarm(voice string, speed int) *speech.Runtime
	ReleaseKeepWarm(voice string) bool
	ResidentVoices() []string
}

// CallSessionInvalidator revokes live phone calls after a credential change.
// It is optional so the web console remains testable without a running call
// bridge.
type CallSessionInvalidator interface {
	InvalidateUserSessions(string)
}

type DeletionCallSessionInvalidator interface {
	InvalidateUserSessionsForDeletion(string)
}

type AccountSyncer interface {
	RefreshAccount(context.Context, string, string) error
}

type AccountSyncStarter interface {
	StartRefreshAccount(context.Context, context.Context, string, string) (int64, error)
	WaitManualSyncs()
}

type ManualSyncCanceler interface {
	CancelManualSync(context.Context, string) (bool, error)
}

// AccountDeletionCoordinator stops a per-account worker before its metadata
// and local files are removed. It is separate from AccountSyncer so small
// test doubles and deployments without mail sync remain valid.
type AccountDeletionCoordinator interface {
	StopAccount(context.Context, string) error
}

type Server struct {
	// JobContext owns background jobs independently of HTTP connections.
	JobContext        context.Context
	Store             *store.Store
	Secrets           *secret.Box
	Log               *slog.Logger
	Sessions          *SessionStore
	SIP               SIPController
	Alerts            AlertService
	Voice             VoiceActivator
	VoiceInstaller    VoiceInstaller
	OfflineMode       bool
	Preview           VoicePreviewer
	SpeechPool        VoiceWarm
	Calls             CallSessionInvalidator
	Sync              AccountSyncer
	DataRoot          string
	VoiceDir          string
	TrustedProxyCIDRs []string
	Ready             func(context.Context) error
	RuntimeStatus     func(context.Context) RuntimeStatus
	// SendRecovery optionally replaces the trusted delivery dependency. It is
	// never controlled by a request; normal wiring uses verified SMTP.
	SendRecovery func(context.Context, mailer.Config, []string, []byte) mailer.SendResult
	// Private transport adapters for isolated provider fixtures. Endpoint policy
	// checks still run first; requests and deployment settings cannot set these.
	imapAccountCheck func(context.Context, imapcheck.Config) ([]string, error)
	smtpAccountCheck func(context.Context, mailer.Config) error
	accountEndpoint  func(context.Context, string, int) (string, error)
	loginMu          sync.Mutex
	logins           map[string]*loginState
	loginActive      int
	loginAccounts    map[string]int
	loginWindow      time.Time
	loginTotal       int
	loginLastReap    time.Time
	loginNow         func() time.Time
	loginCheck       func(string, string) bool
	setupOnce        sync.Once
	setupHashSem     chan struct{}
	setupMu          sync.Mutex
	setupAttempts    map[string]setupAttemptState
	setupHash        func(string) (string, error)
	recoveryMu       sync.Mutex
	recoveryRate     map[string]recoveryRateState
	previewMu        sync.Mutex
	previewJobs      map[string]*previewJob
	voiceMu          sync.Mutex
	voiceJobs        map[string]*voiceInstallJob
	voiceJobCancel   map[string]context.CancelFunc
	voiceOnce        sync.Once
	voiceCtx         context.Context
	voiceCancel      context.CancelFunc
	voiceSem         chan struct{}
	admissionMu      sync.Mutex
	closing          bool
	voiceWG          sync.WaitGroup
	requestWG        sync.WaitGroup
}

type voiceInstallJob struct {
	ID        string
	UserID    string
	Voice     string
	Stage     string
	Status    string
	Progress  int
	Error     string
	UpdatedAt time.Time
}

type loginState struct {
	Failures int
	Until    time.Time
	Seen     time.Time
	Pending  int
}

type recoveryRateState struct {
	Started time.Time
	Count   int
}

type recoveryCandidate struct {
	ID        int64
	UserID    string
	TokenHash string
	ExpiresAt string
}

const (
	recoveryRateWindow  = 15 * time.Minute
	recoverySourceLimit = 20
	recoveryTargetLimit = 5
)

type SessionStore struct {
	mu       sync.Mutex
	byToken  map[string]Session
	byUser   map[string]int
	lastReap time.Time
	now      func() time.Time
}

type Session struct {
	UserID  string
	CSRF    string
	Expires time.Time
}

func NewSessionStore() *SessionStore {
	return &SessionStore{byToken: make(map[string]Session), byUser: make(map[string]int)}
}

func (s *Server) Handler() http.Handler {
	if s.Sessions == nil {
		s.Sessions = NewSessionStore()
	}
	s.voiceOnce.Do(func() {
		base := s.JobContext
		if base == nil {
			base = context.Background()
		}
		s.voiceCtx, s.voiceCancel = context.WithCancel(base)
		s.voiceSem = make(chan struct{}, 1)
		if s.Store != nil {
			if err := s.Store.MarkInterruptedVoiceInstallJobs(context.Background(), time.Now().UTC()); err != nil && s.Log != nil {
				s.Log.Warn("voice job recovery failed", "error", err)
			}
		}
	})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /readyz", s.ready)
	mux.HandleFunc("GET /api/v1", s.apiInfo)
	mux.HandleFunc("POST /api/v1/setup", s.setup)
	mux.HandleFunc("POST /api/v1/login", s.login)
	mux.HandleFunc("POST /api/v1/recovery/request", s.requestRecovery)
	mux.HandleFunc("POST /api/v1/recovery/confirm", s.confirmRecovery)
	mux.HandleFunc("POST /api/v1/logout", s.logout)
	mux.HandleFunc("POST /api/v1/security/password", s.changePassword)
	mux.HandleFunc("POST /api/v1/security/pin", s.changePIN)
	mux.HandleFunc("POST /api/v1/security/2fa/setup", s.setup2FA)
	mux.HandleFunc("POST /api/v1/security/2fa/enable", s.enable2FA)
	mux.HandleFunc("POST /api/v1/security/2fa/disable", s.disable2FA)
	mux.HandleFunc("GET /api/v1/me", s.me)
	mux.HandleFunc("GET /api/v1/status", s.operationalStatus)
	mux.HandleFunc("GET /api/v1/users", s.users)
	mux.HandleFunc("POST /api/v1/users", s.createUser)
	mux.HandleFunc("DELETE /api/v1/users/{id}", s.deleteUser)
	mux.HandleFunc("GET /api/v1/accounts", s.accounts)
	mux.HandleFunc("POST /api/v1/accounts", s.saveAccount)
	mux.HandleFunc("PUT /api/v1/accounts/order", s.reorderAccounts)
	mux.HandleFunc("POST /api/v1/accounts/validate", s.validateAccount)
	mux.HandleFunc("POST /api/v1/accounts/test", s.testAccount)
	mux.HandleFunc("POST /api/v1/accounts/{id}/sync", s.syncAccount)
	mux.HandleFunc("POST /api/v1/accounts/{id}/sync/cancel", s.cancelManualSync)
	mux.HandleFunc("GET /api/v1/accounts/{id}/mutations", s.accountMutations)
	mux.HandleFunc("GET /api/v1/accounts/{id}/folder-policy", s.accountFolderPolicy)
	mux.HandleFunc("PUT /api/v1/accounts/{id}/folder-policy", s.saveAccountFolderPolicy)
	mux.HandleFunc("DELETE /api/v1/accounts/{id}", s.deleteAccount)
	mux.HandleFunc("GET /api/v1/cleanup-jobs", s.cleanupJobs)
	mux.HandleFunc("POST /api/v1/cleanup-jobs/{id}/retry", s.retryCleanupJob)
	mux.HandleFunc("GET /api/v1/contacts", s.contacts)
	mux.HandleFunc("POST /api/v1/contacts", s.createContact)
	mux.HandleFunc("PUT /api/v1/contacts/{id}", s.updateContact)
	mux.HandleFunc("DELETE /api/v1/contacts/{id}", s.deleteContact)
	mux.HandleFunc("GET /api/v1/whitelist", s.whitelist)
	mux.HandleFunc("POST /api/v1/whitelist", s.addWhitelist)
	mux.HandleFunc("DELETE /api/v1/whitelist/{id}", s.deleteWhitelist)
	mux.HandleFunc("GET /api/v1/settings", s.settings)
	mux.HandleFunc("PUT /api/v1/settings", s.saveSettings)
	mux.HandleFunc("GET /api/v1/admin/alerts", s.adminAlerts)
	mux.HandleFunc("PUT /api/v1/admin/alerts", s.saveAdminAlerts)
	mux.HandleFunc("GET /api/v1/alert-numbers", s.alertNumbers)
	mux.HandleFunc("POST /api/v1/alert-numbers", s.saveAlertNumber)
	mux.HandleFunc("PUT /api/v1/alert-numbers/{id}/active", s.activateAlertNumber)
	mux.HandleFunc("DELETE /api/v1/alert-numbers/{id}", s.deleteAlertNumber)
	mux.HandleFunc("GET /api/v1/voices", s.voices)
	mux.HandleFunc("POST /api/v1/voices/install", s.installVoice)
	mux.HandleFunc("GET /api/v1/voices/jobs/active", s.activeVoiceInstallJob)
	mux.HandleFunc("POST /api/v1/voices/jobs/{id}/cancel", s.cancelVoiceInstall)
	mux.HandleFunc("GET /api/v1/voices/jobs/{id}", s.voiceInstallStatus)
	mux.HandleFunc("GET /api/v1/voices/warm", s.voiceWarmStatus)
	mux.HandleFunc("PUT /api/v1/voices/warm/{voice}", s.setVoiceKeepWarm)
	mux.HandleFunc("POST /api/v1/voices/preview", s.previewVoice)
	mux.HandleFunc("GET /api/v1/voices/previews/{id}", s.previewJobStatus)
	mux.HandleFunc("GET /api/v1/voices/previews/{id}/audio", s.previewJobAudio)
	mux.HandleFunc("POST /api/v1/voices/previews/{id}/cancel", s.cancelPreviewJob)
	mux.HandleFunc("POST /api/v1/alerts/test", s.testAlert)
	mux.HandleFunc("GET /api/v1/sip", s.sipSettings)
	mux.HandleFunc("PUT /api/v1/sip", s.saveSIP)
	mux.HandleFunc("GET /assets/{name}", s.asset)
	mux.HandleFunc("GET /", s.index)
	return withRequestID(withSecurityHeaders(s.withAdmission(mux)))
}

func (s *Server) testAlert(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireAlerts(w, r, true)
	if !ok {
		return
	}
	if !s.requireOnline(w, "test alert calling") {
		return
	}
	if s.Alerts == nil {
		writeError(w, http.StatusServiceUnavailable, "the call service is not running")
		return
	}
	if err := s.Alerts.TestAlert(r.Context(), u.ID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "alert_call_placed"})
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	if s.shuttingDown() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "not_ready", "reason": "server is shutting down", "offline_mode": s.OfflineMode})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	runtime := RuntimeStatus{}
	if s.RuntimeStatus != nil {
		runtime = s.RuntimeStatus(ctx)
	}
	writeReady := func(code int, status, reason string) {
		payload := map[string]any{"status": status, "offline_mode": s.OfflineMode}
		if reason != "" {
			payload["reason"] = reason
		}
		if runtime.Process != "" {
			payload["process"] = runtime.Process
		}
		if runtime.Bridge != "" {
			payload["bridge"] = runtime.Bridge
		}
		if runtime.Registration != "" {
			payload["registration"] = runtime.Registration
		}
		if runtime.Media != "" {
			payload["media"] = runtime.Media
		}
		writeJSON(w, code, payload)
	}
	if s.shuttingDown() {
		writeReady(http.StatusServiceUnavailable, "not_ready", "server is shutting down")
		return
	}
	if err := s.Store.Healthy(ctx); err != nil {
		writeReady(http.StatusServiceUnavailable, "not_ready", "database unavailable")
		return
	}
	if s.Ready != nil {
		if err := s.Ready(ctx); err != nil {
			writeReady(http.StatusServiceUnavailable, "not_ready", safeReadinessReason(err.Error()))
			return
		}
	}
	writeReady(http.StatusOK, "ready", "")
}
func (s *Server) apiInfo(w http.ResponseWriter, r *http.Request) {
	setupAvailable := false
	if s.Store != nil {
		count, err := s.Store.UserCount(r.Context())
		if err != nil {
			serverError(w, err)
			return
		}
		setupAvailable = count == 0
	}
	// offline mode is deployment state, not first-run state: it is only
	// disclosed to an authenticated session, while setup_available stays
	// public because the first-run console needs it before any account
	// exists.
	payload := map[string]any{"name": "VOXMail", "api_version": "v1", "setup_available": setupAvailable}
	if _, authed := s.Sessions.Get(sessionToken(r)); authed {
		payload["offline_mode"] = s.OfflineMode
	}
	writeJSON(w, http.StatusOK, payload)
}
func (s *Server) asset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	data, err := consoleAssets.ReadFile("static/assets/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(name, ".svg") {
		w.Header().Set("Content-Type", "image/svg+xml")
	} else {
		w.Header().Set("Content-Type", http.DetectContentType(data))
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(data)
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	setupState := "unknown"
	if s.Store != nil {
		count, err := s.Store.UserCount(r.Context())
		if err == nil {
			setupState = "false"
			if count == 0 {
				setupState = "true"
			}
		}
	}
	html := strings.Replace(string(indexHTML), `data-setup-available="unknown"`, `data-setup-available="`+setupState+`"`, 1)
	_, _ = io.WriteString(w, html)
}

func validateCredentials(username, password, pin string) error {
	if len(strings.TrimSpace(username)) < 3 || len(strings.TrimSpace(username)) > 64 {
		return errors.New("username must be 3-64 characters")
	}
	if err := validatePassword(password); err != nil {
		return err
	}
	return validatePIN(pin)
}

func validatePassword(password string) error {
	if len(password) < 12 {
		return errors.New("password must be at least 12 characters")
	}
	if len([]byte(password)) > auth.MaxPasswordBytes {
		return fmt.Errorf("password must be at most %d bytes", auth.MaxPasswordBytes)
	}
	return nil
}

func validateAccountInput(req accountRequest) error {
	limits := []struct {
		name  string
		value string
		max   int
	}{
		{"account name", req.CanonicalName, 128},
		{"sender name", req.SenderName, 256},
		{"IMAP host", req.IMAPHost, 253},
		{"IMAP username", req.IMAPUser, 320},
		{"IMAP password", req.IMAPPassword, 256},
		{"SMTP host", req.SMTPHost, 253},
		{"SMTP username", req.SMTPUser, 320},
		{"SMTP password", req.SMTPPassword, 256},
	}
	for _, item := range limits {
		if len(item.value) > item.max {
			return fmt.Errorf("%s is too long", item.name)
		}
	}
	if len(req.Email) > 320 || len(req.ID) > 128 || len(req.IMAPSecurity) > 32 || len(req.SMTPSecurity) > 32 {
		return errors.New("account fields are too long")
	}
	if len(req.FolderMap) > 64 || len(req.FolderRoles) > 16 || len(req.AlertFolders) > 64 {
		return errors.New("too many account folders")
	}
	for remote, local := range req.FolderMap {
		if len(remote) > 256 || len(local) > 256 {
			return errors.New("folder mapping is too long")
		}
	}
	for _, folder := range req.AlertFolders {
		if len(folder) > 256 {
			return errors.New("alert folder is too long")
		}
	}
	return nil
}

func validatePIN(pin string) error {
	if len(pin) < 4 || len(pin) > 12 {
		return errors.New("PIN must be 4-12 digits")
	}
	for _, r := range pin {
		if r < '0' || r > '9' {
			return errors.New("PIN must contain digits only")
		}
	}
	return nil
}

func validateFolderMap(mapping map[string]string) error {
	seenRemote := make(map[string]struct{}, len(mapping))
	seenLocal := make(map[string]struct{}, len(mapping))
	for remote, local := range mapping {
		remote, local = strings.TrimSpace(remote), strings.TrimSpace(local)
		if remote == "" || local == "" || path.Clean(strings.ReplaceAll(local, "\\", "/")) == "." || strings.ContainsAny(remote+local, "\x00\r\n") || strings.Contains(remote, "..") || strings.Contains(local, "..") || strings.HasPrefix(remote, "/") || strings.HasPrefix(strings.ReplaceAll(local, "\\", "/"), "/") {
			return errors.New("folder mappings must be relative, non-empty names")
		}
		remoteKey := folderpolicy.RemoteKey(remote)
		if _, ok := seenRemote[remoteKey]; ok {
			return errors.New("folder mappings must have unique remote names")
		}
		localKey := strings.ToLower(path.Clean(strings.ReplaceAll(local, "\\", "/")))
		if _, ok := seenLocal[localKey]; ok {
			return errors.New("folder mappings must have unique local names")
		}
		seenRemote[remoteKey] = struct{}{}
		seenLocal[localKey] = struct{}{}
	}
	return nil
}

// normalizeAlertFolders stores local Maildir folder names because indexed
// messages and alert candidates use local names. An empty selection means the
// documented default: the mapped Inbox, or Inbox when no mapping exists.
func normalizeAlertFolders(folders []string, mapping map[string]string) []string {
	if len(folders) == 0 {
		if inbox := mappedInboxFolder(mapping); inbox != "" {
			return []string{inbox}
		}
		return []string{"Inbox"}
	}
	seen := make(map[string]struct{}, len(folders))
	out := make([]string, 0, len(folders))
	for _, folder := range folders {
		folder = strings.TrimSpace(folder)
		if local := mapping[folder]; local != "" {
			folder = local
		}
		if folder == "" {
			continue
		}
		if _, ok := seen[strings.ToLower(folder)]; ok {
			continue
		}
		seen[strings.ToLower(folder)] = struct{}{}
		out = append(out, folder)
	}
	if len(out) == 0 {
		return []string{mappedInboxFolder(mapping)}
	}
	return out
}

func mappedInboxFolder(mapping map[string]string) string {
	if local := mapping["INBOX"]; local != "" {
		return local
	}
	return "Inbox"
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return false
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return false
	}
	return true
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	// Collection endpoints promise JSON arrays even when their SQL query has
	// no rows. encoding/json's v1 behavior encodes a nil slice as null, which
	// makes a normal empty response incompatible with clients that iterate it.
	// Normalize only a top-level nil slice here; nil fields inside response
	// objects retain their existing semantics.
	if rv := reflect.ValueOf(value); rv.IsValid() && rv.Kind() == reflect.Slice && rv.IsNil() {
		value = reflect.MakeSlice(rv.Type(), 0, 0).Interface()
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

type apiErrorResponse struct {
	Failure   *OperationalFailure `json:"failure,omitempty"`
	Error     string              `json:"error"`
	Code      string              `json:"code"`
	RequestID string              `json:"request_id,omitempty"`
	Fields    map[string]string   `json:"fields,omitempty"`
}

func writeError(w http.ResponseWriter, status int, message string) {
	response := apiErrorResponse{Error: message, Code: apiErrorCode(status), RequestID: w.Header().Get("X-Request-ID")}
	writeJSON(w, status, response)
}

// Validation errors identify a field without echoing submitted credentials.
func writeFieldError(w http.ResponseWriter, field, message string) {
	writeJSON(w, http.StatusBadRequest, apiErrorResponse{Error: message, Code: "bad_request", RequestID: w.Header().Get("X-Request-ID"), Fields: map[string]string{field: message}})
}

func serverError(w http.ResponseWriter, err error) {
	if err != nil {
		slog.Default().Error("http request failed", "request_id", w.Header().Get("X-Request-ID"), "error", redactHTTPError(err))
	}
	writeError(w, http.StatusInternalServerError, "internal server error")
}

func apiErrorCode(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "bad_request"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusConflict:
		return "conflict"
	case http.StatusTooManyRequests:
		return "rate_limited"
	case http.StatusBadGateway:
		return "upstream_failure"
	case http.StatusServiceUnavailable:
		return "unavailable"
	case http.StatusGatewayTimeout:
		return "upstream_timeout"
	default:
		if status >= 500 {
			return "internal_error"
		}
		return "request_failed"
	}
}

func redactHTTPError(err error) string {
	message := strings.TrimSpace(err.Error())
	if message == "" {
		return "unknown error"
	}
	lower := strings.ToLower(message)
	for _, marker := range []string{"password", "passwd", "auth_pass", "authorization", "cookie", "token", "secret", "pin", "otp"} {
		if strings.Contains(lower, marker) {
			return "internal error detail redacted"
		}
	}
	if len(message) > 512 {
		return message[:512] + "…"
	}
	return message
}

func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := newID()
		w.Header().Set("X-Request-ID", requestID)
		if !diagnostics.Enabled() {
			next.ServeHTTP(w, r)
			return
		}
		recorder := &diagnosticResponse{ResponseWriter: w, status: http.StatusOK}
		started := time.Now()
		defer func() {
			// ServeMux patterns contain placeholders; never log user-controlled
			// paths, queries, request bodies, cookies or authorization headers.
			slog.DebugContext(r.Context(), "HTTP request completed", "request_id", requestID, "method", r.Method, "route", r.Pattern, "status", recorder.status, "duration_ms", time.Since(started).Milliseconds())
		}()
		next.ServeHTTP(recorder, r)
	})
}

func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), geolocation=(), microphone=(), payment=(), usb=()")
		w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Pragma", "no-cache")
		}
		next.ServeHTTP(w, r)
	})
}
func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func parseID(value string) int64 {
	var id int64
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0
		}
		if id > (1<<53-9)/10 {
			return 0
		}
		id = id*10 + int64(r-'0')
	}
	return id
}

type diagnosticResponse struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *diagnosticResponse) WriteHeader(code int) {
	if !w.wrote && code >= 200 {
		w.status = code
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(code)
}
func (w *diagnosticResponse) Write(p []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}
func (w *diagnosticResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *diagnosticResponse) Flush()                      { _ = http.NewResponseController(w.ResponseWriter).Flush() }
