package web

import (
	"context"
	"crypto/rand"
	"database/sql"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/voxmail/voxmail/internal/auth"
	"github.com/voxmail/voxmail/internal/imapcheck"
	"github.com/voxmail/voxmail/internal/mailer"
	"github.com/voxmail/voxmail/internal/secret"
	"github.com/voxmail/voxmail/internal/speech"
	"github.com/voxmail/voxmail/internal/store"
)

//go:embed static/index.html
var indexHTML []byte

// SIPController applies deployment SIP changes to the local baresip process.
// It is optional: without it the console still persists SIP settings, but no
// baresip child is managed.
type SIPController interface {
	Apply(context.Context) error
}

// AlertService places outgoing test alert calls on demand. It is optional:
// without it the console reports that the call service is unavailable.
type AlertService interface {
	TestAlert(context.Context, string) error
}

// VoiceActivator installs/warm-ups a trusted voice and refreshes the live
// static prompt set before the voice preference is persisted.
type VoiceActivator interface {
	ActivateVoice(context.Context, string, int, int) error
}

// VoicePreviewer synthesizes a short, bounded sample for an already selected
// voice. It is intentionally separate from VoiceActivator so tests and
// deployments that do not run Piper can omit preview support.
type VoicePreviewer interface {
	PreviewVoice(context.Context, string, int) ([]byte, error)
}

// CallSessionInvalidator revokes live phone calls after a credential change.
// It is optional so the web console remains testable without a running call
// bridge.
type CallSessionInvalidator interface {
	InvalidateUserSessions(string)
}

type AccountSyncer interface {
	RefreshAccount(context.Context, string, string) error
}

type Server struct {
	Store     *store.Store
	Secrets   *secret.Box
	Log       *slog.Logger
	Sessions  *SessionStore
	SIP       SIPController
	Alerts    AlertService
	Voice     VoiceActivator
	Preview   VoicePreviewer
	Calls     CallSessionInvalidator
	Sync      AccountSyncer
	DataRoot  string
	VoiceDir  string
	Ready     func(context.Context) error
	loginMu   sync.Mutex
	logins    map[string]loginState
	voiceMu   sync.Mutex
	voiceJobs map[string]*voiceInstallJob
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
}

type SessionStore struct {
	mu       sync.Mutex
	byToken  map[string]Session
	lastReap time.Time
}

type Session struct {
	UserID  string
	CSRF    string
	Expires time.Time
}

func NewSessionStore() *SessionStore { return &SessionStore{byToken: make(map[string]Session)} }

func (s *Server) Handler() http.Handler {
	if s.Sessions == nil {
		s.Sessions = NewSessionStore()
	}
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
	mux.HandleFunc("GET /api/v1/users", s.users)
	mux.HandleFunc("POST /api/v1/users", s.createUser)
	mux.HandleFunc("DELETE /api/v1/users/{id}", s.deleteUser)
	mux.HandleFunc("GET /api/v1/accounts", s.accounts)
	mux.HandleFunc("POST /api/v1/accounts", s.saveAccount)
	mux.HandleFunc("POST /api/v1/accounts/validate", s.validateAccount)
	mux.HandleFunc("POST /api/v1/accounts/test", s.testAccount)
	mux.HandleFunc("POST /api/v1/accounts/{id}/sync", s.syncAccount)
	mux.HandleFunc("GET /api/v1/accounts/{id}/mutations", s.accountMutations)
	mux.HandleFunc("DELETE /api/v1/accounts/{id}", s.deleteAccount)
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
	mux.HandleFunc("GET /api/v1/voices/jobs/{id}", s.voiceInstallStatus)
	mux.HandleFunc("POST /api/v1/voices/preview", s.previewVoice)
	mux.HandleFunc("POST /api/v1/alerts/test", s.testAlert)
	mux.HandleFunc("GET /api/v1/sip", s.sipSettings)
	mux.HandleFunc("PUT /api/v1/sip", s.saveSIP)
	mux.HandleFunc("GET /", s.index)
	return withSecurityHeaders(mux)
}

type setupRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	PIN      string `json:"pin"`
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	count, err := s.Store.UserCount(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	if count != 0 {
		writeError(w, http.StatusConflict, "setup has already been completed")
		return
	}
	var req setupRequest
	if !decode(w, r, &req) {
		return
	}
	if err := validateCredentials(req.Username, req.Password, req.PIN); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	pw, err := auth.Hash(req.Password)
	if err != nil {
		writeError(w, http.StatusBadRequest, "password cannot be hashed")
		return
	}
	pin, err := auth.Hash(req.PIN)
	if err != nil {
		writeError(w, http.StatusBadRequest, "PIN cannot be hashed")
		return
	}
	id := newID()
	u := store.User{ID: id, Username: strings.TrimSpace(req.Username), PasswordHash: pw, PINHash: pin, Role: "admin", Enabled: true}
	if err := s.Store.CreateUser(r.Context(), u); err != nil {
		serverError(w, err)
		return
	}
	_ = s.Store.Audit(r.Context(), id, "setup_completed", "")
	s.issueSession(w, r, id)
	writeJSON(w, http.StatusCreated, publicUser(u))
}

type loginRequest struct {
	Username   string `json:"username"`
	Password   string `json:"password"`
	TOTP       string `json:"totp"`
	BackupCode string `json:"backup_code"`
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !decode(w, r, &req) {
		return
	}
	loginKey := r.RemoteAddr + "|" + strings.ToLower(strings.TrimSpace(req.Username))
	if retry := s.loginBlocked(loginKey); retry > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "too many sign-in attempts; try again later")
		return
	}
	u, err := s.Store.UserByUsername(r.Context(), strings.TrimSpace(req.Username))
	if err != nil || !u.Enabled || !auth.Check(u.PasswordHash, req.Password) {
		s.loginFailure(loginKey)
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	totpSecret := s.openTOTPSecret(u.TOTPSecret)
	if totpSecret != "" && !auth.TOTP(totpSecret, req.TOTP, time.Now().UTC()) {
		if !s.consumeBackupCode(r.Context(), u.ID, req.BackupCode) {
			s.loginFailure(loginKey)
			writeError(w, http.StatusUnauthorized, "authenticator or backup code required")
			return
		}
	}
	s.loginSuccess(loginKey)
	_ = s.Store.Audit(r.Context(), u.ID, "login_succeeded", "")
	s.issueSession(w, r, u.ID)
	writeJSON(w, http.StatusOK, publicUser(u))
}

type recoveryRequest struct {
	Email     string `json:"email"`
	AccountID string `json:"account_id"`
	Purpose   string `json:"purpose"`
}

// requestRecovery deliberately returns the same response whether the address
// is known or not. Mailbox ownership is the alternate factor, so this path is
// rate limited and never exposes account enumeration information.
func (s *Server) requestRecovery(w http.ResponseWriter, r *http.Request) {
	var req recoveryRequest
	if !decode(w, r, &req) {
		return
	}
	req.Email = strings.TrimSpace(req.Email)
	if req.Purpose != "reset_password" && req.Purpose != "bypass_2fa" {
		writeError(w, http.StatusBadRequest, "invalid recovery purpose")
		return
	}
	requested := map[string]string{"status": "If the address is configured, a recovery code will be sent."}
	if _, err := mail.ParseAddress(req.Email); err != nil {
		writeJSON(w, http.StatusAccepted, requested)
		return
	}
	accounts, err := s.Store.ListAllAccounts(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	var account store.Account
	for _, candidate := range accounts {
		if req.AccountID != "" && candidate.ID != req.AccountID {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(candidate.Email), req.Email) {
			account = candidate
			break
		}
	}
	if account.ID == "" {
		writeJSON(w, http.StatusAccepted, requested)
		return
	}
	var recent int
	_ = s.Store.DB.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM recovery_tokens WHERE user_id=? AND created_at >= ?`, account.UserID, time.Now().Add(-15*time.Minute).UTC().Format(time.RFC3339Nano)).Scan(&recent)
	if recent >= 5 {
		writeJSON(w, http.StatusAccepted, requested)
		return
	}
	code, err := recoveryCode()
	if err != nil {
		serverError(w, err)
		return
	}
	hash, err := auth.Hash(code)
	if err != nil {
		serverError(w, err)
		return
	}
	now := time.Now().UTC()
	if _, err := s.Store.DB.ExecContext(r.Context(), `UPDATE recovery_tokens SET used_at=? WHERE user_id=? AND purpose=? AND used_at IS NULL`, now.Format(time.RFC3339Nano), account.UserID, req.Purpose); err != nil {
		serverError(w, err)
		return
	}
	if _, err := s.Store.DB.ExecContext(r.Context(), `INSERT INTO recovery_tokens(user_id,email,token_hash,purpose,expires_at,created_at) VALUES(?,?,?,?,?,?)`, account.UserID, strings.ToLower(req.Email), hash, req.Purpose, now.Add(10*time.Minute).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		serverError(w, err)
		return
	}
	if s.Secrets == nil {
		err = errors.New("secret store is unavailable")
	}
	var password string
	if err == nil {
		password, err = s.Secrets.Open(account.SMTPPassword)
	}
	if err == nil {
		body := fmt.Sprintf("Your VOXMail recovery code is %s. It expires in 10 minutes. If you did not request this, ignore this message.", code)
		raw := mailer.BuildMessageWithAttachments(account.Email, account.SenderName, []string{account.Email}, nil, nil, "VOXMail recovery code", body, nil)
		if raw != nil {
			err = mailer.Send(mailer.Config{Host: account.SMTPHost, Port: account.SMTPPort, Security: account.SMTPSecurity, Username: account.SMTPUser, Password: password, From: account.Email}, []string{account.Email}, raw)
		}
	}
	if err != nil {
		// A failed delivery must not leave a valid recovery token behind.
		_, _ = s.Store.DB.ExecContext(context.Background(), `UPDATE recovery_tokens SET used_at=? WHERE token_hash=?`, time.Now().UTC().Format(time.RFC3339Nano), hash)
		if s.Log != nil {
			s.Log.Warn("recovery code delivery failed", "error", err)
		}
	} else {
		_ = s.Store.Audit(r.Context(), account.UserID, "recovery_requested", req.Purpose)
	}
	writeJSON(w, http.StatusAccepted, requested)
}

type recoveryConfirmRequest struct {
	Email       string `json:"email"`
	Purpose     string `json:"purpose"`
	Code        string `json:"code"`
	NewPassword string `json:"new_password"`
}

func (s *Server) confirmRecovery(w http.ResponseWriter, r *http.Request) {
	var req recoveryConfirmRequest
	if !decode(w, r, &req) {
		return
	}
	req.Email = strings.TrimSpace(req.Email)
	if req.Purpose != "reset_password" && req.Purpose != "bypass_2fa" {
		writeError(w, http.StatusBadRequest, "invalid recovery purpose")
		return
	}
	if req.Purpose == "reset_password" && len(req.NewPassword) < 12 {
		writeError(w, http.StatusBadRequest, "password must be at least 12 characters")
		return
	}
	if len(strings.TrimSpace(req.Code)) < 6 {
		writeError(w, http.StatusUnauthorized, "invalid or expired recovery code")
		return
	}
	tx, err := s.Store.DB.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(r.Context(), `SELECT id,user_id,token_hash,expires_at,attempts FROM recovery_tokens WHERE lower(email)=lower(?) AND purpose=? AND used_at IS NULL ORDER BY id DESC LIMIT 5`, req.Email, req.Purpose)
	if err != nil {
		serverError(w, err)
		return
	}
	var tokenID int64
	var userID string
	for rows.Next() {
		var id, attempts int64
		var candidateUser, tokenHash, expires string
		if err := rows.Scan(&id, &candidateUser, &tokenHash, &expires, &attempts); err != nil {
			rows.Close()
			serverError(w, err)
			return
		}
		if attempts >= 5 {
			continue
		}
		_, _ = tx.ExecContext(r.Context(), `UPDATE recovery_tokens SET attempts=attempts+1 WHERE id=?`, id)
		if parsed, parseErr := time.Parse(time.RFC3339Nano, expires); parseErr != nil || time.Now().UTC().After(parsed) {
			_, _ = tx.ExecContext(r.Context(), `UPDATE recovery_tokens SET used_at=? WHERE id=?`, time.Now().UTC().Format(time.RFC3339Nano), id)
			continue
		}
		if auth.Check(tokenHash, strings.TrimSpace(req.Code)) {
			tokenID, userID = id, candidateUser
			break
		}
	}
	rows.Close()
	if tokenID == 0 {
		_ = tx.Commit()
		writeError(w, http.StatusUnauthorized, "invalid or expired recovery code")
		return
	}
	if _, err := tx.ExecContext(r.Context(), `UPDATE recovery_tokens SET used_at=? WHERE id=? AND used_at IS NULL`, time.Now().UTC().Format(time.RFC3339Nano), tokenID); err != nil {
		serverError(w, err)
		return
	}
	if req.Purpose == "reset_password" {
		hash, hashErr := auth.Hash(req.NewPassword)
		if hashErr != nil {
			serverError(w, hashErr)
			return
		}
		if _, err := tx.ExecContext(r.Context(), `UPDATE users SET password_hash=? WHERE id=?`, hash, userID); err != nil {
			serverError(w, err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		serverError(w, err)
		return
	}
	if req.Purpose == "reset_password" {
		s.Sessions.DeleteUser(userID)
		_ = s.Store.Audit(r.Context(), userID, "password_recovered", "mailbox OTP")
		writeJSON(w, http.StatusOK, map[string]string{"status": "password_reset"})
		return
	}
	u, err := s.Store.UserByID(r.Context(), userID)
	if err != nil {
		serverError(w, err)
		return
	}
	s.issueSession(w, r, userID)
	_ = s.Store.Audit(r.Context(), userID, "two_factor_bypassed", "mailbox OTP")
	writeJSON(w, http.StatusOK, map[string]any{"status": "signed_in", "user": publicUser(u)})
}

func recoveryCode() (string, error) {
	var data [4]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", binary.BigEndian.Uint32(data[:])%1000000), nil
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if token := sessionToken(r); token != "" {
		s.Sessions.Delete(token)
	}
	http.SetCookie(w, &http.Cookie{Name: "voxmail_session", MaxAge: -1, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
	writeJSON(w, http.StatusOK, map[string]string{"status": "signed_out"})
}

type passwordChangeRequest struct {
	Current string `json:"current_password"`
	New     string `json:"new_password"`
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, true)
	if !ok {
		return
	}
	var req passwordChangeRequest
	if !decode(w, r, &req) {
		return
	}
	if !auth.Check(u.PasswordHash, req.Current) {
		writeError(w, http.StatusUnauthorized, "current password is incorrect")
		return
	}
	if len(req.New) < 12 {
		writeError(w, http.StatusBadRequest, "password must be at least 12 characters")
		return
	}
	hash, err := auth.Hash(req.New)
	if err != nil {
		serverError(w, err)
		return
	}
	if _, err := s.Store.DB.ExecContext(r.Context(), `UPDATE users SET password_hash=? WHERE id=?`, hash, u.ID); err != nil {
		serverError(w, err)
		return
	}
	_ = s.Store.Audit(r.Context(), u.ID, "password_changed", "")
	s.Sessions.DeleteUser(u.ID)
	s.issueSession(w, r, u.ID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "changed"})
}

type pinChangeRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPIN          string `json:"new_pin"`
}

func (s *Server) changePIN(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, true)
	if !ok {
		return
	}
	var req pinChangeRequest
	if !decode(w, r, &req) {
		return
	}
	if !auth.Check(u.PasswordHash, req.CurrentPassword) {
		writeError(w, http.StatusUnauthorized, "current password is incorrect")
		return
	}
	if err := validatePIN(req.NewPIN); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	hash, err := auth.Hash(req.NewPIN)
	if err != nil {
		serverError(w, err)
		return
	}
	if _, err := s.Store.DB.ExecContext(r.Context(), `UPDATE users SET pin_hash=? WHERE id=?`, hash, u.ID); err != nil {
		serverError(w, err)
		return
	}
	if s.Calls != nil {
		s.Calls.InvalidateUserSessions(u.ID)
	}
	_ = s.Store.Audit(r.Context(), u.ID, "pin_changed", "")
	writeJSON(w, http.StatusOK, map[string]string{"status": "changed"})
}

type twoFARequest struct {
	CurrentPassword string `json:"current_password"`
	Secret          string `json:"secret"`
	Code            string `json:"code"`
}

func (s *Server) setup2FA(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, true)
	if !ok {
		return
	}
	var req struct {
		CurrentPassword string `json:"current_password"`
	}
	if !decode(w, r, &req) {
		return
	}
	if !auth.Check(u.PasswordHash, req.CurrentPassword) {
		writeError(w, http.StatusUnauthorized, "current password is incorrect")
		return
	}
	secretValue, err := auth.GenerateTOTPSecret()
	if err != nil {
		serverError(w, err)
		return
	}
	label := url.QueryEscape("VOXMail:" + u.Username)
	issuer := url.QueryEscape("VOXMail")
	uri := "otpauth://totp/" + label + "?secret=" + secretValue + "&issuer=" + issuer
	writeJSON(w, http.StatusOK, map[string]string{"secret": secretValue, "provisioning_uri": uri})
}

func (s *Server) enable2FA(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, true)
	if !ok {
		return
	}
	var req twoFARequest
	if !decode(w, r, &req) {
		return
	}
	if s.Secrets == nil || !auth.Check(u.PasswordHash, req.CurrentPassword) || !auth.TOTP(req.Secret, req.Code, time.Now().UTC()) {
		writeError(w, http.StatusUnauthorized, "password or authenticator code is incorrect")
		return
	}
	codes := make([]string, 0, 10)
	hashes := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		code, err := auth.RandomToken(5)
		if err != nil {
			serverError(w, err)
			return
		}
		hash, err := auth.Hash(code)
		if err != nil {
			serverError(w, err)
			return
		}
		codes = append(codes, code)
		hashes = append(hashes, hash)
	}
	sealed, err := s.Secrets.Seal(strings.TrimSpace(req.Secret))
	if err != nil {
		serverError(w, err)
		return
	}
	tx, err := s.Store.DB.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(r.Context(), `UPDATE users SET totp_secret=?,backup_codes='' WHERE id=?`, sealed, u.ID); err != nil {
		serverError(w, err)
		return
	}
	if _, err := tx.ExecContext(r.Context(), `DELETE FROM totp_backup_codes WHERE user_id=?`, u.ID); err != nil {
		serverError(w, err)
		return
	}
	created := time.Now().UTC().Format(time.RFC3339Nano)
	for _, hash := range hashes {
		if _, err := tx.ExecContext(r.Context(), `INSERT INTO totp_backup_codes(user_id,code_hash,created_at) VALUES(?,?,?)`, u.ID, hash, created); err != nil {
			serverError(w, err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		serverError(w, err)
		return
	}
	_ = s.Store.Audit(r.Context(), u.ID, "two_factor_enabled", "")
	writeJSON(w, http.StatusOK, map[string]any{"status": "enabled", "backup_codes": codes})
}

func (s *Server) disable2FA(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, true)
	if !ok {
		return
	}
	var req twoFARequest
	if !decode(w, r, &req) {
		return
	}
	if u.TOTPSecret == "" || !auth.Check(u.PasswordHash, req.CurrentPassword) || !auth.TOTP(s.openTOTPSecret(u.TOTPSecret), req.Code, time.Now().UTC()) {
		writeError(w, http.StatusUnauthorized, "password or authenticator code is incorrect")
		return
	}
	tx, err := s.Store.DB.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(r.Context(), `UPDATE users SET totp_secret='',backup_codes='' WHERE id=?`, u.ID); err != nil {
		serverError(w, err)
		return
	}
	if _, err := tx.ExecContext(r.Context(), `DELETE FROM totp_backup_codes WHERE user_id=?`, u.ID); err != nil {
		serverError(w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, err)
		return
	}
	_ = s.Store.Audit(r.Context(), u.ID, "two_factor_disabled", "")
	writeJSON(w, http.StatusOK, map[string]string{"status": "disabled"})
}

func (s *Server) openTOTPSecret(value string) string {
	if value == "" {
		return ""
	}
	if s.Secrets != nil {
		if plain, err := s.Secrets.Open(value); err == nil {
			return plain
		}
	}
	// Accept legacy plaintext secrets so an upgrade does not lock users out;
	// newly enabled secrets are always sealed above.
	return value
}

func (s *Server) consumeBackupCode(ctx context.Context, userID, candidate string) bool {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return false
	}
	tx, err := s.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return false
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id,code_hash FROM totp_backup_codes WHERE user_id=? AND used_at IS NULL ORDER BY id`, userID)
	if err != nil {
		return false
	}
	var matchedID int64
	for rows.Next() {
		var id int64
		var hash string
		if scanErr := rows.Scan(&id, &hash); scanErr != nil {
			rows.Close()
			return false
		}
		if auth.Check(hash, candidate) {
			matchedID = id
			break
		}
	}
	rows.Close()
	if matchedID != 0 {
		result, updateErr := tx.ExecContext(ctx, `UPDATE totp_backup_codes SET used_at=? WHERE id=? AND user_id=? AND used_at IS NULL`, time.Now().UTC().Format(time.RFC3339Nano), matchedID, userID)
		if updateErr != nil {
			return false
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return false
		}
	} else {
		// One-time compatibility path for databases created before the
		// normalized backup-code table existed. Successful use migrates the
		// remaining legacy hashes into the table before deleting the old blob.
		var raw string
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(backup_codes,'') FROM users WHERE id=?`, userID).Scan(&raw); err != nil {
			return false
		}
		var hashes []string
		if json.Unmarshal([]byte(raw), &hashes) != nil {
			return false
		}
		legacyIndex := -1
		for i, hash := range hashes {
			if auth.Check(hash, candidate) {
				legacyIndex = i
				break
			}
		}
		if legacyIndex < 0 {
			return false
		}
		hashes = append(hashes[:legacyIndex], hashes[legacyIndex+1:]...)
		for _, hash := range hashes {
			if _, err := tx.ExecContext(ctx, `INSERT INTO totp_backup_codes(user_id,code_hash,created_at) VALUES(?,?,?)`, userID, hash, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
				return false
			}
		}
		updated, err := json.Marshal(hashes)
		if err != nil {
			return false
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET backup_codes=? WHERE id=?`, string(updated), userID); err != nil {
			return false
		}
	}
	if err := tx.Commit(); err != nil {
		return false
	}
	_ = s.Store.Audit(ctx, userID, "backup_code_used", "")
	return true
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, false)
	if ok {
		writeJSON(w, http.StatusOK, publicUser(u))
	}
}

func (s *Server) users(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	users, err := s.Store.ListUsers(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	out := make([]map[string]any, 0, len(users))
	for _, u := range users {
		out = append(out, publicUser(u))
	}
	writeJSON(w, http.StatusOK, out)
}

type userRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	PIN      string `json:"pin"`
	Role     string `json:"role"`
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	var req userRequest
	if !decode(w, r, &req) {
		return
	}
	if err := validateCredentials(req.Username, req.Password, req.PIN); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	pw, err := auth.Hash(req.Password)
	if err != nil {
		writeError(w, http.StatusBadRequest, "password cannot be hashed")
		return
	}
	pin, err := auth.Hash(req.PIN)
	if err != nil {
		writeError(w, http.StatusBadRequest, "PIN cannot be hashed")
		return
	}
	role := "user"
	if req.Role == "admin" {
		role = "admin"
	}
	u := store.User{ID: newID(), Username: strings.TrimSpace(req.Username), PasswordHash: pw, PINHash: pin, Role: role, Enabled: true}
	if err := s.Store.CreateUser(r.Context(), u); err != nil {
		writeError(w, http.StatusConflict, "username already exists")
		return
	}
	writeJSON(w, http.StatusCreated, publicUser(u))
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	admin, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	if r.PathValue("id") == admin.ID {
		writeError(w, http.StatusBadRequest, "admin cannot delete itself")
		return
	}
	if err := s.Store.DeleteUser(r.Context(), r.PathValue("id")); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type accountRequest struct {
	ID                            string            `json:"id"`
	CanonicalName                 string            `json:"canonical_name"`
	Email                         string            `json:"email"`
	SenderName                    string            `json:"sender_name"`
	IMAPHost                      string            `json:"imap_host"`
	IMAPUser                      string            `json:"imap_user"`
	IMAPPassword                  string            `json:"imap_password"`
	IMAPSecurity                  string            `json:"imap_security"`
	SMTPHost                      string            `json:"smtp_host"`
	SMTPUser                      string            `json:"smtp_user"`
	SMTPPassword                  string            `json:"smtp_password"`
	SMTPSecurity                  string            `json:"smtp_security"`
	IMAPPort                      int               `json:"imap_port"`
	SMTPPort                      int               `json:"smtp_port"`
	SyncIntervalMinutes           int               `json:"sync_interval_minutes"`
	ReconciliationIntervalMinutes int               `json:"reconciliation_interval_minutes"`
	DisplayOrder                  int               `json:"display_order"`
	FolderMap                     map[string]string `json:"folder_map"`
	FolderRoles                   map[string]string `json:"folder_roles"`
	AlertFolders                  []string          `json:"alert_folders"`
	InitialCutoff                 *string           `json:"initial_cutoff,omitempty"`
	RetentionDays                 *int              `json:"retention_days,omitempty"`
	CallAlertEnabled              bool              `json:"call_alert_enabled"`
}

type accountView struct {
	ID                            string            `json:"id"`
	CanonicalName                 string            `json:"canonical_name"`
	Email                         string            `json:"email"`
	SenderName                    string            `json:"sender_name"`
	IMAPHost                      string            `json:"imap_host"`
	IMAPPort                      int               `json:"imap_port"`
	IMAPSecurity                  string            `json:"imap_security"`
	IMAPUser                      string            `json:"imap_user"`
	SMTPHost                      string            `json:"smtp_host"`
	SMTPPort                      int               `json:"smtp_port"`
	SMTPSecurity                  string            `json:"smtp_security"`
	SMTPUser                      string            `json:"smtp_user"`
	FolderMap                     map[string]string `json:"folder_map"`
	FolderRoles                   map[string]string `json:"folder_roles"`
	AlertFolders                  []string          `json:"alert_folders"`
	SyncIntervalMinutes           int               `json:"sync_interval_minutes"`
	ReconciliationIntervalMinutes int               `json:"reconciliation_interval_minutes"`
	DisplayOrder                  int               `json:"display_order"`
	InitialCutoff                 *string           `json:"initial_cutoff,omitempty"`
	RetentionDays                 *int              `json:"retention_days,omitempty"`
	CallAlertEnabled              bool              `json:"call_alert_enabled"`
	LastSync                      *store.SyncRun    `json:"last_sync,omitempty"`
}

func accountJSON(a store.Account) accountView {
	v := accountView{ID: a.ID, CanonicalName: a.CanonicalName, Email: a.Email, SenderName: a.SenderName, IMAPHost: a.IMAPHost, IMAPPort: a.IMAPPort, IMAPSecurity: a.IMAPSecurity, IMAPUser: a.IMAPUser, SMTPHost: a.SMTPHost, SMTPPort: a.SMTPPort, SMTPSecurity: a.SMTPSecurity, SMTPUser: a.SMTPUser, SyncIntervalMinutes: a.SyncIntervalMinutes, ReconciliationIntervalMinutes: a.ReconciliationIntervalMinutes, DisplayOrder: a.DisplayOrder, InitialCutoff: a.InitialCutoff, RetentionDays: a.RetentionDays, CallAlertEnabled: a.CallAlertEnabled}
	if err := json.Unmarshal([]byte(a.FolderMap), &v.FolderMap); err != nil || v.FolderMap == nil {
		v.FolderMap = map[string]string{}
	}
	if err := json.Unmarshal([]byte(a.AlertFolders), &v.AlertFolders); err != nil || v.AlertFolders == nil {
		v.AlertFolders = []string{}
	}
	return v
}

func (s *Server) accounts(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, false)
	if !ok {
		return
	}
	accounts, err := s.Store.ListAccounts(r.Context(), u.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	out := make([]accountView, 0, len(accounts))
	for _, account := range accounts {
		view := accountJSON(account)
		view.FolderRoles = make(map[string]string)
		if roles, err := s.Store.ListFolderRoles(r.Context(), u.ID, account.ID); err == nil {
			for _, role := range roles {
				view.FolderRoles[role.Role] = role.RemotePath
			}
		}
		if run, err := s.Store.LatestSyncRun(r.Context(), account.ID, false); err == nil {
			view.LastSync = &run
		}
		out = append(out, view)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) saveAccount(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, true)
	if !ok {
		return
	}
	var req accountRequest
	if !decode(w, r, &req) {
		return
	}
	alertsAvailable, err := s.Store.AlertsAvailable(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	if !alertsAvailable && u.Role != "admin" {
		req.CallAlertEnabled = false
		req.AlertFolders = nil
	}
	if req.CanonicalName == "" || req.Email == "" || req.IMAPHost == "" || req.IMAPUser == "" || req.SMTPHost == "" || req.SMTPUser == "" {
		writeError(w, http.StatusBadRequest, "canonical name, email, IMAP, and SMTP fields are required")
		return
	}
	if _, err := mail.ParseAddress(req.Email); err != nil {
		writeError(w, http.StatusBadRequest, "email address is invalid")
		return
	}
	if req.IMAPPort < 0 || req.IMAPPort > 65535 || req.SMTPPort < 0 || req.SMTPPort > 65535 || req.SMTPPort == 25 {
		writeError(w, http.StatusBadRequest, "mail ports must be between 1 and 65535; SMTP port 25 requires an explicit plaintext policy")
		return
	}
	if req.DisplayOrder < 0 || req.SyncIntervalMinutes < 0 || req.ReconciliationIntervalMinutes < 0 || (req.RetentionDays != nil && *req.RetentionDays < 1) {
		writeError(w, http.StatusBadRequest, "order, sync interval, and retention values are invalid")
		return
	}
	if req.InitialCutoff != nil && strings.TrimSpace(*req.InitialCutoff) != "" {
		if _, err := time.Parse(time.RFC3339, strings.TrimSpace(*req.InitialCutoff)); err != nil {
			writeError(w, http.StatusBadRequest, "initial cutoff must be RFC3339")
			return
		}
	}
	if req.FolderMap == nil {
		req.FolderMap = make(map[string]string)
	}
	for role, remote := range req.FolderRoles {
		role = strings.ToLower(strings.TrimSpace(role))
		remote = strings.TrimSpace(remote)
		if role != "inbox" && role != "sent" && role != "drafts" && role != "spam" && role != "trash" && role != "archive" {
			writeError(w, http.StatusBadRequest, "invalid folder role")
			return
		}
		if remote == "" {
			continue
		}
		if req.FolderMap[remote] == "" {
			name := role
			if len(name) > 0 {
				name = strings.ToUpper(name[:1]) + name[1:]
			}
			req.FolderMap[remote] = name
		}
	}
	if err := validateFolderMap(req.FolderMap); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	for _, folder := range req.AlertFolders {
		if strings.TrimSpace(folder) == "" || strings.ContainsAny(folder, "\x00\r\n") {
			writeError(w, http.StatusBadRequest, "alert folders must be non-empty names")
			return
		}
	}
	req.AlertFolders = normalizeAlertFolders(req.AlertFolders, req.FolderMap)
	if req.ID != "" {
		accounts, err := s.Store.ListAccounts(r.Context(), u.ID)
		if err != nil {
			serverError(w, err)
			return
		}
		found := false
		for _, account := range accounts {
			if account.ID == req.ID {
				found = true
				break
			}
		}
		if !found {
			writeError(w, http.StatusNotFound, "account not found")
			return
		}
	} else if req.IMAPPassword == "" || req.SMTPPassword == "" {
		writeError(w, http.StatusBadRequest, "IMAP and SMTP passwords are required for a new account")
		return
	}
	if req.IMAPPort == 0 {
		req.IMAPPort = 993
	}
	if req.SMTPPort == 0 {
		req.SMTPPort = 465
	}
	var securityErr error
	req.IMAPSecurity, securityErr = normalizeMailSecurity(req.IMAPSecurity, req.IMAPPort)
	if securityErr != nil {
		writeError(w, http.StatusBadRequest, "invalid IMAP security mode")
		return
	}
	req.SMTPSecurity, securityErr = normalizeMailSecurity(req.SMTPSecurity, req.SMTPPort)
	if securityErr != nil {
		writeError(w, http.StatusBadRequest, "invalid SMTP security mode")
		return
	}
	if req.SyncIntervalMinutes < 1 {
		req.SyncIntervalMinutes = 5
	}
	if req.ReconciliationIntervalMinutes < 1 {
		req.ReconciliationIntervalMinutes = 1440
	}
	folder, _ := json.Marshal(req.FolderMap)
	alerts, _ := json.Marshal(req.AlertFolders)
	id := req.ID
	if id == "" {
		id = newID()
	}
	a := store.Account{ID: id, UserID: u.ID, CanonicalName: req.CanonicalName, Email: req.Email, SenderName: req.SenderName, IMAPHost: req.IMAPHost, IMAPPort: req.IMAPPort, IMAPSecurity: req.IMAPSecurity, IMAPUser: req.IMAPUser, IMAPPassword: req.IMAPPassword, SMTPHost: req.SMTPHost, SMTPPort: req.SMTPPort, SMTPSecurity: req.SMTPSecurity, SMTPUser: req.SMTPUser, SMTPPassword: req.SMTPPassword, FolderMap: string(folder), AlertFolders: string(alerts), SyncIntervalMinutes: req.SyncIntervalMinutes, ReconciliationIntervalMinutes: req.ReconciliationIntervalMinutes, DisplayOrder: req.DisplayOrder, InitialCutoff: req.InitialCutoff, RetentionDays: req.RetentionDays, CallAlertEnabled: req.CallAlertEnabled}
	if err := s.Store.SaveAccount(r.Context(), s.Secrets, a); err != nil {
		serverError(w, err)
		return
	}
	if req.FolderRoles != nil {
		if err := s.Store.SaveFolderRoles(r.Context(), u.ID, id, req.FolderRoles, req.FolderMap); err != nil {
			serverError(w, err)
			return
		}
	}
	_ = s.Store.Audit(r.Context(), u.ID, "account_saved", id)
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

// validateAccount authenticates a new account without persisting credentials.
// The browser wizard calls this before POST /accounts so folder roles can be
// chosen only after the provider has been reached and its folders discovered.
func (s *Server) validateAccount(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.require(w, r, true); !ok {
		return
	}
	var req accountRequest
	if !decode(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.IMAPPassword) == "" || strings.TrimSpace(req.SMTPPassword) == "" {
		writeError(w, http.StatusBadRequest, "IMAP and SMTP passwords are required for validation")
		return
	}
	if req.IMAPHost == "" || req.IMAPUser == "" || req.SMTPHost == "" || req.SMTPUser == "" {
		writeError(w, http.StatusBadRequest, "IMAP and SMTP hosts and usernames are required")
		return
	}
	if req.IMAPPort == 0 {
		req.IMAPPort = 993
	}
	if req.SMTPPort == 0 {
		req.SMTPPort = 465
	}
	var err error
	if req.IMAPSecurity, err = normalizeMailSecurity(req.IMAPSecurity, req.IMAPPort); err != nil {
		writeError(w, http.StatusBadRequest, "invalid IMAP security mode")
		return
	}
	if req.SMTPSecurity, err = normalizeMailSecurity(req.SMTPSecurity, req.SMTPPort); err != nil {
		writeError(w, http.StatusBadRequest, "invalid SMTP security mode")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	folders, err := s.checkAccountCredentials(ctx, req)
	if err != nil {
		if s.log() != nil {
			s.log().Warn("new account validation failed", "imap_host", req.IMAPHost, "smtp_host", req.SMTPHost, "error", err)
		}
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "authenticated", "folders": folders})
}

func (s *Server) checkAccountCredentials(ctx context.Context, req accountRequest) ([]string, error) {
	imapAddress, err := ssrfSafeAddress(ctx, req.IMAPHost, req.IMAPPort)
	if err != nil {
		return nil, fmt.Errorf("IMAP host is not reachable by policy")
	}
	folders, err := imapcheck.Check(ctx, imapcheck.Config{Host: req.IMAPHost, Address: imapAddress, Port: req.IMAPPort, Security: req.IMAPSecurity, Username: req.IMAPUser, Password: req.IMAPPassword})
	if err != nil {
		return nil, err
	}
	smtpAddress, err := ssrfSafeAddress(ctx, req.SMTPHost, req.SMTPPort)
	if err != nil {
		return nil, fmt.Errorf("SMTP host is not reachable by policy")
	}
	if err := mailer.Check(ctx, mailer.Config{Host: req.SMTPHost, Address: smtpAddress, Port: req.SMTPPort, Security: req.SMTPSecurity, Username: req.SMTPUser, Password: req.SMTPPassword}); err != nil {
		return nil, err
	}
	return folders, nil
}

func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, true)
	if ok {
		accountID := r.PathValue("id")
		if err := s.Store.DeleteAccount(r.Context(), u.ID, accountID); err != nil {
			serverError(w, err)
			return
		}
		if s.DataRoot != "" {
			root := filepath.Join(s.DataRoot, "mail")
			maildir := filepath.Join(root, r.PathValue("id"))
			if rel, err := filepath.Rel(root, maildir); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				if err := os.RemoveAll(maildir); err != nil && s.log() != nil {
					s.log().Warn("could not remove account maildir", "path", maildir, "error", err)
				}
			}
		}
		_ = s.Store.Audit(r.Context(), u.ID, "account_deleted", accountID)
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) syncAccount(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, true)
	if !ok {
		return
	}
	if s.Sync == nil {
		writeError(w, http.StatusServiceUnavailable, "mail synchronization is not running")
		return
	}
	accountID := r.PathValue("id")
	accounts, err := s.Store.ListAccounts(r.Context(), u.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	found := false
	for _, account := range accounts {
		if account.ID == accountID {
			found = true
			break
		}
	}
	if !found {
		writeError(w, http.StatusNotFound, "account not found")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	if err := s.Sync.RefreshAccount(ctx, u.ID, accountID); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	_ = s.Store.Audit(r.Context(), u.ID, "account_sync_requested", accountID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "synchronized"})
}

func (s *Server) accountMutations(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, false)
	if !ok {
		return
	}
	accountID := strings.TrimSpace(r.PathValue("id"))
	if accountID == "" {
		writeError(w, http.StatusBadRequest, "account id is required")
		return
	}
	accounts, err := s.Store.ListAccounts(r.Context(), u.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	found := false
	for _, account := range accounts {
		if account.ID == accountID {
			found = true
			break
		}
	}
	if !found {
		writeError(w, http.StatusNotFound, "account not found")
		return
	}
	mutations, err := s.Store.ListMessageMutations(r.Context(), u.ID, accountID, 50)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, mutations)
}

type testAccountRequest struct {
	AccountID string `json:"account_id"`
}

func (s *Server) testAccount(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, true)
	if !ok {
		return
	}
	var req testAccountRequest
	if !decode(w, r, &req) {
		return
	}
	if req.AccountID == "" {
		writeError(w, http.StatusBadRequest, "account id is required")
		return
	}
	accounts, err := s.Store.ListAccounts(r.Context(), u.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	var account store.Account
	for _, candidate := range accounts {
		if candidate.ID == req.AccountID {
			account = candidate
			break
		}
	}
	if account.ID == "" {
		writeError(w, http.StatusNotFound, "account not found")
		return
	}
	if s.Secrets == nil {
		writeError(w, http.StatusServiceUnavailable, "mail secret storage is unavailable")
		return
	}
	imapPassword, err := s.Secrets.Open(account.IMAPPassword)
	if err != nil {
		writeError(w, http.StatusBadGateway, "stored IMAP credentials are unavailable")
		return
	}
	smtpPassword, err := s.Secrets.Open(account.SMTPPassword)
	if err != nil {
		writeError(w, http.StatusBadGateway, "stored SMTP credentials are unavailable")
		return
	}
	imapPort := account.IMAPPort
	if imapPort == 0 {
		imapPort = 993
	}
	smtpPort := account.SMTPPort
	if smtpPort == 0 {
		smtpPort = 465
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	imapAddress, err := ssrfSafeAddress(ctx, account.IMAPHost, imapPort)
	if err != nil {
		writeError(w, http.StatusBadGateway, "IMAP host is not reachable by policy")
		return
	}
	folders, err := imapcheck.Check(ctx, imapcheck.Config{Host: account.IMAPHost, Address: imapAddress, Port: imapPort, Security: account.IMAPSecurity, Username: account.IMAPUser, Password: imapPassword})
	if err != nil {
		if s.log() != nil {
			s.log().Warn("authenticated IMAP test failed", "account_id", account.ID, "error", err)
		}
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	smtpAddress, err := ssrfSafeAddress(ctx, account.SMTPHost, smtpPort)
	if err != nil {
		writeError(w, http.StatusBadGateway, "SMTP host is not reachable by policy")
		return
	}
	if err := mailer.Check(ctx, mailer.Config{Host: account.SMTPHost, Address: smtpAddress, Port: smtpPort, Security: account.SMTPSecurity, Username: account.SMTPUser, Password: smtpPassword}); err != nil {
		if s.log() != nil {
			s.log().Warn("authenticated SMTP test failed", "account_id", account.ID, "error", err)
		}
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "authenticated", "folders": folders})
}

// ssrfSafeDial resolves host and connects to a validated, globally routable
// address so the glance test can never be redirected at loopback, private,
// link-local, metadata, or CGNAT ranges.
func ssrfSafeDial(ctx context.Context, host string, port int) error {
	address, err := ssrfSafeAddress(ctx, host, port)
	if err != nil {
		return err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return err
	}
	return conn.Close()
}

func ssrfSafeAddress(ctx context.Context, host string, port int) (string, error) {
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return "", err
	}
	var dialErr error
	for _, resolved := range ips {
		if ssrfBlocked(resolved.IP) {
			continue
		}
		deadline, _ := ctx.Deadline()
		timeout := 5 * time.Second
		if remaining := time.Until(deadline); remaining > 0 {
			timeout = remaining
		}
		conn, dialErr := net.DialTimeout("tcp", net.JoinHostPort(resolved.IP.String(), strconv.Itoa(port)), timeout)
		if dialErr == nil {
			_ = conn.Close()
			return net.JoinHostPort(resolved.IP.String(), strconv.Itoa(port)), nil
		}
	}
	if len(ips) == 0 {
		return "", errors.New("no addresses resolved")
	}
	if dialErr != nil {
		return "", dialErr
	}
	return "", errors.New("host resolves only to blocked addresses")
}

func ssrfBlocked(ip net.IP) bool {
	if ip4 := ip.To4(); ip4 != nil {
		ip = ip4
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		if v4[0] == 0 || v4[0] >= 224 {
			return true
		}
		if v4[0] == 100 && v4[1]&0xc0 == 0x40 {
			return true
		}
		return false
	}
	return false
}

func (s *Server) contacts(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, false)
	if ok {
		out, err := s.Store.ListContacts(r.Context(), u.ID)
		if err != nil {
			serverError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}
func (s *Server) createContact(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, true)
	if !ok {
		return
	}
	var c store.Contact
	if !decode(w, r, &c) {
		return
	}
	c.UserID = u.ID
	if strings.TrimSpace(c.Name) == "" || strings.TrimSpace(c.Email) == "" {
		writeError(w, http.StatusBadRequest, "name and email are required")
		return
	}
	id, err := s.Store.AddContact(r.Context(), c)
	if err != nil {
		writeError(w, http.StatusConflict, "contact already exists")
		return
	}
	c.ID = id
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) updateContact(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, true)
	if !ok {
		return
	}
	c := store.Contact{ID: parseID(r.PathValue("id"))}
	if c.ID < 1 || !decode(w, r, &c) {
		if c.ID < 1 {
			writeError(w, http.StatusBadRequest, "invalid contact id")
		}
		return
	}
	c.UserID = u.ID
	if strings.TrimSpace(c.Name) == "" || strings.TrimSpace(c.Email) == "" {
		writeError(w, http.StatusBadRequest, "name and email are required")
		return
	}
	if err := s.Store.UpdateContact(r.Context(), c); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusBadRequest, "contact not found")
			return
		}
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}
func (s *Server) deleteContact(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, true)
	if !ok {
		return
	}
	id := parseID(r.PathValue("id"))
	if id < 1 {
		writeError(w, http.StatusBadRequest, "invalid contact id")
		return
	}
	if err := s.Store.DeleteContact(r.Context(), u.ID, id); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) whitelist(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, false)
	if !ok {
		return
	}
	out, err := s.Store.ListWhitelist(r.Context(), u.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
func (s *Server) addWhitelist(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, true)
	if !ok {
		return
	}
	var e store.WhitelistEntry
	if !decode(w, r, &e) {
		return
	}
	e.UserID = u.ID
	e.Phone = normalizePhone(e.Phone)
	if e.Phone == "" {
		writeError(w, http.StatusBadRequest, "phone is required")
		return
	}
	if err := s.Store.AddWhitelist(r.Context(), e); err != nil {
		writeError(w, http.StatusConflict, "phone already belongs to a user")
		return
	}
	_ = s.Store.Audit(r.Context(), u.ID, "caller_whitelist_added", e.Phone)
	writeJSON(w, http.StatusCreated, e)
}
func (s *Server) deleteWhitelist(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, true)
	if !ok {
		return
	}
	id := parseID(r.PathValue("id"))
	if id < 1 {
		writeError(w, http.StatusBadRequest, "invalid whitelist id")
		return
	}
	if err := s.Store.DeleteWhitelist(r.Context(), u.ID, id); err != nil {
		serverError(w, err)
		return
	}
	_ = s.Store.Audit(r.Context(), u.ID, "caller_whitelist_deleted", fmt.Sprint(id))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, false)
	if !ok {
		return
	}
	var voice string
	var menu, email int
	var enabled int
	var phone *string
	err := s.Store.DB.QueryRowContext(r.Context(), `SELECT tts_voice,menu_speed,email_speed,alerts_enabled,alert_phone FROM settings WHERE user_id=?`, u.ID).Scan(&voice, &menu, &email, &enabled, &phone)
	if err != nil {
		serverError(w, err)
		return
	}
	available, err := s.Store.AlertsAvailable(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	result := map[string]any{"tts_voice": voice, "menu_speed": menu, "email_speed": email, "alerts_available": available}
	if available || u.Role == "admin" {
		result["alerts_enabled"] = enabled != 0
		result["alert_phone"] = phone
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) adminAlerts(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	available, err := s.Store.AlertsAvailable(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"available": available})
}

func (s *Server) saveAdminAlerts(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	var body struct {
		Available bool `json:"available"`
	}
	if !decode(w, r, &body) {
		return
	}
	if err := s.Store.SetAlertsAvailable(r.Context(), body.Available); err != nil {
		serverError(w, err)
		return
	}
	_ = s.Store.Audit(r.Context(), u.ID, "global_alert_availability_changed", fmt.Sprintf("available=%t", body.Available))
	writeJSON(w, http.StatusOK, body)
}
func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, true)
	if !ok {
		return
	}
	var body struct {
		TTSVoice      string  `json:"tts_voice"`
		MenuSpeed     int     `json:"menu_speed"`
		EmailSpeed    int     `json:"email_speed"`
		AlertsEnabled bool    `json:"alerts_enabled"`
		AlertPhone    *string `json:"alert_phone"`
	}
	if !decode(w, r, &body) {
		return
	}
	alertsAvailable, err := s.Store.AlertsAvailable(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	if !alertsAvailable && u.Role != "admin" {
		body.AlertsEnabled = false
		body.AlertPhone = nil
	}
	if body.TTSVoice == "" {
		body.TTSVoice = "en_US-hfc_male-medium"
	}
	if body.MenuSpeed < 1 || body.MenuSpeed > 5 {
		body.MenuSpeed = 3
	}
	if body.EmailSpeed < 1 || body.EmailSpeed > 5 {
		body.EmailSpeed = 2
	}
	body.TTSVoice = strings.TrimSpace(body.TTSVoice)
	if body.TTSVoice == "" || strings.ContainsAny(body.TTSVoice, "/\\\x00\r\n") || strings.Contains(body.TTSVoice, "..") {
		writeError(w, http.StatusBadRequest, "invalid voice model")
		return
	}
	if !s.voiceAvailable(body.TTSVoice) {
		writeError(w, http.StatusBadRequest, "voice model is not installed; an administrator must install it first")
		return
	}
	if s.Voice != nil {
		voiceCtx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
		err := s.Voice.ActivateVoice(voiceCtx, body.TTSVoice, body.MenuSpeed, body.EmailSpeed)
		cancel()
		if err != nil {
			writeError(w, http.StatusBadGateway, "voice installation or prompt generation failed")
			return
		}
	}
	// Selecting a trusted voice is configuration, not proof that the model is
	// already present.  The model may be installed immediately through the
	// console or provisioned asynchronously by deployment tooling; rejecting
	// the setting here made a valid selection impossible on a fresh volume.
	// Calls still fail closed with a clear speech error until the model exists.
	if body.AlertPhone != nil {
		phone := normalizePhone(*body.AlertPhone)
		if phone == "" {
			body.AlertPhone = nil
		} else {
			body.AlertPhone = &phone
		}
	}
	_, err = s.Store.DB.ExecContext(r.Context(), `INSERT INTO settings(user_id,tts_voice,menu_speed,email_speed,alerts_enabled,alert_phone) VALUES(?,?,?,?,?,?) ON CONFLICT(user_id) DO UPDATE SET tts_voice=excluded.tts_voice,menu_speed=excluded.menu_speed,email_speed=excluded.email_speed,alerts_enabled=excluded.alerts_enabled,alert_phone=excluded.alert_phone`, u.ID, body.TTSVoice, body.MenuSpeed, body.EmailSpeed, body.AlertsEnabled, body.AlertPhone)
	if err != nil {
		serverError(w, err)
		return
	}
	if body.AlertPhone == nil {
		if err := s.Store.ClearAlertNumbers(r.Context(), u.ID); err != nil {
			serverError(w, err)
			return
		}
	} else if err := s.Store.SaveAlertNumber(r.Context(), u.ID, *body.AlertPhone, true); err != nil {
		serverError(w, err)
		return
	}
	result := map[string]any{
		"tts_voice":   body.TTSVoice,
		"menu_speed":  body.MenuSpeed,
		"email_speed": body.EmailSpeed,
	}
	if alertsAvailable || u.Role == "admin" {
		result["alerts_enabled"] = body.AlertsEnabled
		result["alert_phone"] = body.AlertPhone
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) alertNumbers(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireAlerts(w, r, false)
	if !ok {
		return
	}
	numbers, err := s.Store.ListAlertNumbers(r.Context(), u.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	// Migrate the legacy single-number setting into the durable list on first
	// read, preserving existing deployments without exposing two sources of
	// truth to the browser.
	if len(numbers) == 0 {
		if legacy, legacyErr := s.Store.ActiveAlertNumber(r.Context(), u.ID); legacyErr == nil && legacy != "" {
			if saveErr := s.Store.SaveAlertNumber(r.Context(), u.ID, legacy, true); saveErr == nil {
				numbers, _ = s.Store.ListAlertNumbers(r.Context(), u.ID)
			}
		}
	}
	writeJSON(w, http.StatusOK, numbers)
}

func (s *Server) saveAlertNumber(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireAlerts(w, r, true)
	if !ok {
		return
	}
	var body struct {
		Number string `json:"number"`
		Active bool   `json:"active"`
	}
	if !decode(w, r, &body) {
		return
	}
	body.Number = normalizePhone(body.Number)
	if body.Number == "" {
		writeError(w, http.StatusBadRequest, "a valid alert number is required")
		return
	}
	if err := s.Store.SaveAlertNumber(r.Context(), u.ID, body.Number, body.Active); err != nil {
		serverError(w, err)
		return
	}
	_ = s.Store.Audit(r.Context(), u.ID, "alert_number_saved", body.Number)
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

func (s *Server) activateAlertNumber(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireAlerts(w, r, true)
	if !ok {
		return
	}
	id := parseID(r.PathValue("id"))
	if id < 1 {
		writeError(w, http.StatusBadRequest, "invalid alert number id")
		return
	}
	if err := s.Store.SetActiveAlertNumber(r.Context(), u.ID, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "alert number not found")
		} else {
			serverError(w, err)
		}
		return
	}
	_ = s.Store.Audit(r.Context(), u.ID, "alert_number_activated", fmt.Sprint(id))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteAlertNumber(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireAlerts(w, r, true)
	if !ok {
		return
	}
	id := parseID(r.PathValue("id"))
	if id < 1 {
		writeError(w, http.StatusBadRequest, "invalid alert number id")
		return
	}
	if err := s.Store.DeleteAlertNumber(r.Context(), u.ID, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "alert number not found")
		} else {
			serverError(w, err)
		}
		return
	}
	_ = s.Store.Audit(r.Context(), u.ID, "alert_number_deleted", fmt.Sprint(id))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) voices(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, false)
	if !ok {
		return
	}
	result := make([]map[string]any, 0)
	installed := speech.InstalledVoiceNames(s.VoiceDir)
	if u.Role != "admin" {
		for _, name := range installed {
			result = append(result, map[string]any{"voice": name, "installed": true, "downloadable": false})
		}
		writeJSON(w, http.StatusOK, result)
		return
	}
	seen := make(map[string]bool, len(installed))
	for _, name := range installed {
		seen[name] = true
	}
	for _, voice := range speech.TrustedVoiceCatalog() {
		result = append(result, map[string]any{"voice": voice.Voice, "installed": seen[voice.Voice], "downloadable": true})
		seen[voice.Voice] = true
	}
	for _, name := range installed {
		if _, trusted := speech.FindTrustedVoice(name); trusted {
			continue
		}
		result = append(result, map[string]any{"voice": name, "installed": true, "downloadable": false})
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) installVoice(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	if s.VoiceDir == "" {
		writeError(w, http.StatusServiceUnavailable, "voice directory is not configured")
		return
	}
	var body struct {
		Voice string `json:"voice"`
	}
	if !decode(w, r, &body) {
		return
	}
	if _, ok := speech.FindTrustedVoice(body.Voice); !ok {
		writeError(w, http.StatusBadRequest, "voice model is not in the trusted catalog")
		return
	}
	job := &voiceInstallJob{ID: newID(), UserID: u.ID, Voice: body.Voice, Stage: "queued", Status: "queued", Progress: 0, UpdatedAt: time.Now().UTC()}
	s.voiceMu.Lock()
	if s.voiceJobs == nil {
		s.voiceJobs = make(map[string]*voiceInstallJob)
	}
	s.voiceJobs[job.ID] = job
	s.voiceMu.Unlock()
	var menuSpeed, emailSpeed int = 3, 2
	_ = s.Store.DB.QueryRowContext(r.Context(), `SELECT menu_speed,email_speed FROM settings WHERE user_id=?`, u.ID).Scan(&menuSpeed, &emailSpeed)
	go s.runVoiceInstall(job.ID, u.ID, body.Voice, menuSpeed, emailSpeed)
	writeJSON(w, http.StatusAccepted, map[string]string{"job_id": job.ID, "status": "queued"})
}

func (s *Server) updateVoiceJob(id, stage, status string, progress int, jobErr error) {
	s.voiceMu.Lock()
	defer s.voiceMu.Unlock()
	job := s.voiceJobs[id]
	if job == nil {
		return
	}
	job.Stage, job.Status, job.Progress, job.UpdatedAt = stage, status, progress, time.Now().UTC()
	if jobErr != nil {
		job.Error = "voice installation failed"
	}
}

func (s *Server) runVoiceInstall(id, userID, voice string, menuSpeed, emailSpeed int) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	s.updateVoiceJob(id, "installing", "running", 10, nil)
	var err error
	if s.Voice != nil {
		s.updateVoiceJob(id, "activating", "running", 35, nil)
		err = s.Voice.ActivateVoice(ctx, voice, menuSpeed, emailSpeed)
	} else {
		err = speech.InstallVoice(ctx, s.VoiceDir, voice)
	}
	if err != nil {
		s.updateVoiceJob(id, "failed", "failed", 100, err)
		return
	}
	s.updateVoiceJob(id, "complete", "complete", 100, nil)
	_ = s.Store.Audit(context.Background(), userID, "voice_installed", voice)
}

func (s *Server) voiceInstallStatus(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, false)
	if !ok {
		return
	}
	id := r.PathValue("id")
	s.voiceMu.Lock()
	job := s.voiceJobs[id]
	if job == nil || (job.UserID != u.ID && u.Role != "admin") {
		s.voiceMu.Unlock()
		writeError(w, http.StatusNotFound, "voice installation job not found")
		return
	}
	copy := *job
	s.voiceMu.Unlock()
	result := map[string]any{"job_id": copy.ID, "voice": copy.Voice, "stage": copy.Stage, "status": copy.Status, "progress": copy.Progress, "updated_at": copy.UpdatedAt}
	if copy.Error != "" {
		result["error"] = copy.Error
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) voiceAvailable(name string) bool {
	if s.VoiceDir == "" {
		_, ok := speech.FindTrustedVoice(name)
		return ok
	}
	return speech.IsInstalledVoice(s.VoiceDir, name)
}

func (s *Server) previewVoice(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.require(w, r, true); !ok {
		return
	}
	if s.Preview == nil {
		writeError(w, http.StatusServiceUnavailable, "voice preview is unavailable")
		return
	}
	var body struct {
		Voice string `json:"voice"`
		Speed int    `json:"speed"`
	}
	if !decode(w, r, &body) {
		return
	}
	body.Voice = strings.TrimSpace(body.Voice)
	if !s.voiceAvailable(body.Voice) {
		writeError(w, http.StatusBadRequest, "voice model is not installed")
		return
	}
	if body.Speed < 1 || body.Speed > 5 {
		body.Speed = 3
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	audio, err := s.Preview.PreviewVoice(ctx, body.Voice, body.Speed)
	if err != nil {
		writeError(w, http.StatusBadGateway, "voice preview failed")
		return
	}
	if len(audio) == 0 || len(audio) > 16<<20 {
		writeError(w, http.StatusBadGateway, "voice preview output is invalid")
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(audio)
}

func (s *Server) testAlert(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireAlerts(w, r, true)
	if !ok {
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

func (s *Server) sipSettings(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	_ = u
	st, err := s.Store.GetSIP(r.Context())
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		serverError(w, err)
		return
	}
	if errors.Is(err, sql.ErrNoRows) {
		st = store.SIPSettings{Port: 5060, Transport: "udp", RegInterval: 300}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"domain":       st.Domain,
		"username":     st.Username,
		"port":         st.Port,
		"transport":    st.Transport,
		"reg_interval": st.RegInterval,
		"enabled":      st.Enabled,
		"password_set": st.Password != "",
	})
}

func (s *Server) saveSIP(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	var body struct {
		Domain      string `json:"domain"`
		Username    string `json:"username"`
		Password    string `json:"password"`
		Port        int    `json:"port"`
		Transport   string `json:"transport"`
		RegInterval int    `json:"reg_interval"`
		Enabled     bool   `json:"enabled"`
	}
	if !decode(w, r, &body) {
		return
	}
	body.Domain = strings.TrimSpace(body.Domain)
	body.Username = strings.TrimSpace(body.Username)
	if body.Port < 1 || body.Port > 65535 {
		writeError(w, http.StatusBadRequest, "SIP port must be between 1 and 65535")
		return
	}
	switch body.Transport {
	case "udp", "tcp", "tls":
	default:
		body.Transport = "udp"
	}
	if body.RegInterval < 0 || body.RegInterval > 86400 {
		writeError(w, http.StatusBadRequest, "registration interval must be between 0 and 86400 seconds")
		return
	}
	if body.Enabled {
		if body.Domain == "" || body.Username == "" {
			writeError(w, http.StatusBadRequest, "domain and username are required to enable SIP")
			return
		}
		if !validSIPHost(body.Domain) {
			writeError(w, http.StatusBadRequest, "SIP domain is invalid")
			return
		}
		if !validSIPUsername(body.Username) {
			writeError(w, http.StatusBadRequest, "SIP username contains invalid characters")
			return
		}
	}
	st := store.SIPSettings{Domain: body.Domain, Username: body.Username, Password: body.Password, Port: body.Port, Transport: body.Transport, RegInterval: body.RegInterval, Enabled: body.Enabled}
	if err := s.Store.UpsertSIP(r.Context(), s.Secrets, st); err != nil {
		serverError(w, err)
		return
	}
	if s.SIP != nil {
		// Apply must not inherit the request context: baresip would be killed
		// the moment the handler returns. Use an uncancelled ctx instead.
		if err := s.SIP.Apply(context.WithoutCancel(r.Context())); err != nil {
			s.log().Error("sip settings applied but baresip restart failed", "error", err)
			writeError(w, http.StatusInternalServerError, "SIP settings saved, but the call client could not be restarted")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}
func (s *Server) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func validSIPHost(host string) bool {
	if host == "" || strings.ContainsAny(host, " \t\r\n;/\x00\\\"") || strings.Contains(host, "..") {
		return false
	}
	if strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") {
		return false
	}
	for _, r := range host {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '.' && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

func validSIPUsername(username string) bool {
	if username == "" {
		return false
	}
	for _, r := range username {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '+' && r != '-' && r != '.' && r != '_' && r != '~' {
			return false
		}
	}
	return true
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.Store.Healthy(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
		return
	}
	if s.Ready != nil {
		if err := s.Ready(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready", "reason": err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
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
	writeJSON(w, http.StatusOK, map[string]any{"name": "VOXMail", "api_version": "v1", "setup_available": setupAvailable})
}
func (s *Server) index(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

func (s *Server) require(w http.ResponseWriter, r *http.Request, write bool) (store.User, bool) {
	token := sessionToken(r)
	session, ok := s.Sessions.Get(token)
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return store.User{}, false
	}
	if write && r.Method != "GET" && r.Method != "HEAD" && r.Header.Get("X-CSRF-Token") != session.CSRF {
		writeError(w, http.StatusForbidden, "csrf token required")
		return store.User{}, false
	}
	user, err := s.Store.UserByID(r.Context(), session.UserID)
	if err != nil || !user.Enabled {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return store.User{}, false
	}
	return user, true
}
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) (store.User, bool) {
	u, ok := s.require(w, r, r.Method != "GET")
	if !ok {
		return u, false
	}
	if u.Role != "admin" {
		writeError(w, http.StatusForbidden, "administrator access required")
		return store.User{}, false
	}
	return u, true
}

// requireAlerts keeps the alert API unavailable when the administrator has
// disabled the feature for the deployment. The separate admin availability
// endpoint remains usable so the administrator can turn it back on.
func (s *Server) requireAlerts(w http.ResponseWriter, r *http.Request, write bool) (store.User, bool) {
	u, ok := s.require(w, r, write)
	if !ok {
		return store.User{}, false
	}
	available, err := s.Store.AlertsAvailable(r.Context())
	if err != nil {
		serverError(w, err)
		return store.User{}, false
	}
	if !available {
		writeError(w, http.StatusNotFound, "alert controls are disabled by the administrator")
		return store.User{}, false
	}
	return u, true
}

func (s *Server) issueSession(w http.ResponseWriter, r *http.Request, userID string) {
	token := newID()
	csrf := newID()
	s.Sessions.Put(token, Session{UserID: userID, CSRF: csrf, Expires: time.Now().Add(12 * time.Hour)})
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	http.SetCookie(w, &http.Cookie{Name: "voxmail_session", Value: token, Path: "/", HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode, MaxAge: 43200})
	w.Header().Set("X-CSRF-Token", csrf)
}
func sessionToken(r *http.Request) string {
	c, err := r.Cookie("voxmail_session")
	if err != nil {
		return ""
	}
	return c.Value
}
func (s *SessionStore) Put(token string, session Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byToken[token] = session
	s.reapLocked()
}
func (s *SessionStore) Get(token string) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.byToken[token]
	if !ok || time.Now().After(session.Expires) {
		delete(s.byToken, token)
		return Session{}, false
	}
	s.reapLocked()
	return session, true
}
func (s *SessionStore) Delete(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byToken, token)
}

func (s *SessionStore) DeleteUser(userID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for token, session := range s.byToken {
		if session.UserID == userID {
			delete(s.byToken, token)
		}
	}
}
func (s *SessionStore) reapLocked() {
	if len(s.byToken) < 256 || time.Since(s.lastReap) < time.Minute {
		return
	}
	s.lastReap = time.Now()
	now := time.Now()
	for token, session := range s.byToken {
		if now.After(session.Expires) {
			delete(s.byToken, token)
		}
	}
}

func (s *Server) loginBlocked(key string) time.Duration {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	s.reapLoginsLocked()
	state := s.logins[key]
	if time.Now().Before(state.Until) {
		return time.Until(state.Until)
	}
	return 0
}

func (s *Server) loginFailure(key string) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	s.reapLoginsLocked()
	if s.logins == nil {
		s.logins = make(map[string]loginState)
	}
	state := s.logins[key]
	state.Failures++
	state.Seen = time.Now()
	if state.Failures >= 5 {
		state.Until = time.Now().Add(5 * time.Minute)
		state.Failures = 0
	}
	s.logins[key] = state
}

func (s *Server) loginSuccess(key string) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	delete(s.logins, key)
}

func (s *Server) reapLoginsLocked() {
	if len(s.logins) < 256 {
		return
	}
	cutoff := time.Now().Add(-1 * time.Hour)
	for key, state := range s.logins {
		if state.Seen.Before(cutoff) && !time.Now().Before(state.Until) {
			delete(s.logins, key)
		}
	}
}
func publicUser(u store.User) map[string]any {
	return map[string]any{"id": u.ID, "username": u.Username, "role": u.Role, "enabled": u.Enabled, "two_factor_enabled": u.TOTPSecret != ""}
}
func validateCredentials(username, password, pin string) error {
	if len(strings.TrimSpace(username)) < 3 || len(strings.TrimSpace(username)) > 64 {
		return errors.New("username must be 3-64 characters")
	}
	if len(password) < 12 {
		return errors.New("password must be at least 12 characters")
	}
	return validatePIN(pin)
}

func normalizeMailSecurity(value string, port int) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		if port == 993 || port == 465 {
			return "implicit_tls", nil
		}
		return "starttls", nil
	}
	if value != "implicit_tls" && value != "starttls" {
		return "", errors.New("security must use implicit TLS or STARTTLS")
	}
	return value, nil
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
	seen := make(map[string]struct{}, len(mapping))
	for remote, local := range mapping {
		remote, local = strings.TrimSpace(remote), strings.TrimSpace(local)
		if remote == "" || local == "" || strings.ContainsAny(remote+local, "\x00\r\n") || strings.Contains(remote, "..") || strings.Contains(local, "..") || strings.HasPrefix(remote, "/") || strings.HasPrefix(local, "/") {
			return errors.New("folder mappings must be relative, non-empty names")
		}
		if _, ok := seen[local]; ok {
			return errors.New("folder mappings must have unique local names")
		}
		seen[local] = struct{}{}
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
	return true
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
func serverError(w http.ResponseWriter, _ error) {
	writeError(w, http.StatusInternalServerError, "internal server error")
}
func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; frame-src 'none'; media-src 'self' blob:; object-src 'none'")
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

func normalizePhone(value string) string {
	value = strings.TrimSpace(value)
	if at := strings.IndexByte(value, '@'); at >= 0 {
		value = value[:at]
	}
	if colon := strings.LastIndexByte(value, ':'); colon >= 0 {
		value = value[colon+1:]
	}
	if semi := strings.IndexByte(value, ';'); semi >= 0 {
		value = value[:semi]
	}
	return value
}
