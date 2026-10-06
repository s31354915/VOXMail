package web

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/voxmail/voxmail/internal/auth"
	"github.com/voxmail/voxmail/internal/lifecycle"
	"github.com/voxmail/voxmail/internal/mailer"
	"github.com/voxmail/voxmail/internal/store"
)

type setupRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	PIN      string `json:"pin"`
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
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
	if err := s.Store.CreateBootstrapUser(r.Context(), u); err != nil {
		if errors.Is(err, store.ErrSetupCompleted) {
			writeError(w, http.StatusConflict, "setup has already been completed")
			return
		}
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
	if len(strings.TrimSpace(req.Username)) > 64 || len([]byte(req.Password)) > auth.MaxPasswordBytes || len(req.TOTP) > 64 || len(req.BackupCode) > 128 {
		writeError(w, http.StatusBadRequest, "credential fields are too long")
		return
	}
	keys := loginKeysForHost(s.clientHost(r), req.Username)
	for _, key := range keys {
		if retry := s.loginBlocked(key); retry > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
			writeError(w, http.StatusTooManyRequests, "too many sign-in attempts; try again later")
			return
		}
	}
	u, err := s.Store.UserByUsername(r.Context(), strings.TrimSpace(req.Username))
	if err != nil || !u.Enabled || !auth.Check(u.PasswordHash, req.Password) {
		for _, key := range keys {
			s.loginFailure(key)
		}
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	totpSecret := s.openTOTPSecret(u.TOTPSecret)
	if totpSecret != "" {
		step, valid := auth.TOTPWithStep(totpSecret, req.TOTP, time.Now().UTC())
		if valid {
			if !s.consumeTOTPReplay(r.Context(), u.ID, step) {
				for _, key := range keys {
					s.loginFailure(key)
				}
				writeError(w, http.StatusUnauthorized, "authenticator or backup code required")
				return
			}
		} else if !s.consumeBackupCode(r.Context(), u.ID, req.BackupCode) {
			for _, key := range keys {
				s.loginFailure(key)
			}
			writeError(w, http.StatusUnauthorized, "authenticator or backup code required")
			return
		}
	}
	for _, key := range keys {
		s.loginSuccess(key)
	}
	_ = s.Store.Audit(r.Context(), u.ID, "login_succeeded", "")
	s.issueSession(w, r, u.ID)
	writeJSON(w, http.StatusOK, publicUser(u))
}

type recoveryRequest struct {
	Email     string `json:"email"`
	AccountID string `json:"account_id"`
	Purpose   string `json:"purpose"`
}

func (s *Server) allowRecoveryRequest(r *http.Request, email string) bool {
	host := s.clientHost(r)
	if host == "" {
		host = "unknown"
	}
	target := strings.ToLower(strings.TrimSpace(email))
	now := time.Now()
	s.recoveryMu.Lock()
	defer s.recoveryMu.Unlock()
	if s.recoveryRate == nil {
		s.recoveryRate = make(map[string]recoveryRateState)
	}
	if len(s.recoveryRate) > 4096 {
		for key, state := range s.recoveryRate {
			if now.Sub(state.Started) >= recoveryRateWindow {
				delete(s.recoveryRate, key)
			}
		}
		for len(s.recoveryRate) > 4096 {
			for key := range s.recoveryRate {
				delete(s.recoveryRate, key)
				break
			}
		}
	}
	allowed := true
	for _, bucket := range []struct {
		key   string
		limit int
	}{
		{key: "source:" + host, limit: recoverySourceLimit},
		{key: "target:" + target, limit: recoveryTargetLimit},
	} {
		state := s.recoveryRate[bucket.key]
		if state.Started.IsZero() || now.Sub(state.Started) >= recoveryRateWindow {
			state = recoveryRateState{Started: now}
		}
		if state.Count >= bucket.limit {
			allowed = false
		} else {
			state.Count++
		}
		s.recoveryRate[bucket.key] = state
	}
	return allowed
}

func (s *Server) reserveRecoveryToken(ctx context.Context, userID, email, purpose string, expiresAt, createdAt time.Time) (int64, string, bool, error) {
	return s.Store.ReserveRecoveryToken(ctx, userID, email, purpose, expiresAt, createdAt, recoveryRateWindow, recoveryTargetLimit)
}

func (s *Server) setRecoveryTokenHash(ctx context.Context, id int64, placeholder, hash string) error {
	return s.Store.SetRecoveryTokenHash(ctx, id, placeholder, hash)
}

func (s *Server) invalidateRecoveryToken(ctx context.Context, id int64) {
	cleanupCtx, cleanupCancel := lifecycle.CleanupContext(ctx)
	defer cleanupCancel()
	_ = s.Store.InvalidateRecoveryToken(cleanupCtx, id, time.Now().UTC())
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
	if len(req.Email) > 320 || len(req.AccountID) > 128 {
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "If the address is configured, a recovery code will be sent."})
		return
	}
	if req.Purpose != "reset_password" && req.Purpose != "bypass_2fa" {
		writeError(w, http.StatusBadRequest, "invalid recovery purpose")
		return
	}
	requested := map[string]string{"status": "If the address is configured, a recovery code will be sent."}
	parsed, err := mail.ParseAddress(req.Email)
	if err != nil {
		writeJSON(w, http.StatusAccepted, requested)
		return
	}
	// Rate-limit and match the canonical mailbox, not a caller-controlled
	// display name or casing variant.
	req.Email = strings.ToLower(strings.TrimSpace(parsed.Address))
	if !s.allowRecoveryRequest(r, req.Email) {
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
	user, err := s.Store.UserByID(r.Context(), account.UserID)
	if err != nil || !user.Enabled {
		writeJSON(w, http.StatusAccepted, requested)
		return
	}
	code, err := recoveryCode()
	if err != nil {
		serverError(w, err)
		return
	}
	now := time.Now().UTC()
	tokenID, placeholder, reserved, err := s.reserveRecoveryToken(r.Context(), account.UserID, req.Email, req.Purpose, now.Add(10*time.Minute), now)
	if err != nil {
		serverError(w, err)
		return
	}
	if !reserved {
		writeJSON(w, http.StatusAccepted, requested)
		return
	}
	hash, err := auth.Hash(code)
	if err != nil {
		s.invalidateRecoveryToken(r.Context(), tokenID)
		serverError(w, err)
		return
	}
	if err := s.setRecoveryTokenHash(r.Context(), tokenID, placeholder, hash); err != nil {
		if errors.Is(err, store.ErrRecoveryReservationSuperseded) {
			// A newer request for the same purpose intentionally invalidated this
			// reservation while hashing. It is not an internal failure and must
			// preserve the generic recovery response.
			writeJSON(w, http.StatusAccepted, requested)
			return
		}
		s.invalidateRecoveryToken(r.Context(), tokenID)
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
		raw, buildErr := mailer.BuildMessageWithAttachmentsE(account.Email, account.SenderName, []string{account.Email}, nil, nil, "VOXMail recovery code", body, nil)
		if buildErr != nil {
			err = fmt.Errorf("recovery message could not be constructed: %w", buildErr)
		} else {
			result := mailer.SendWithOutcome(mailer.Config{Host: account.SMTPHost, Port: account.SMTPPort, Security: account.SMTPSecurity, Username: account.SMTPUser, Password: password, From: account.Email}, []string{account.Email}, raw)
			if result.Status != mailer.SendAccepted {
				err = result.Err
			} else if result.CleanupError != nil && s.Log != nil {
				s.Log.Warn("recovery SMTP accepted message but QUIT cleanup failed", "error", result.CleanupError)
			}
		}
	}
	if err != nil {
		// A failed delivery must not leave a valid recovery token behind.
		s.invalidateRecoveryToken(r.Context(), tokenID)
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

func (s *Server) takeRecoveryCandidate(ctx context.Context, email, purpose string, skipped map[int64]struct{}) (recoveryCandidate, bool, error) {
	candidate, ok, err := s.Store.TakeRecoveryCandidate(ctx, email, purpose, skipped, time.Now().UTC(), 5)
	return recoveryCandidate{ID: candidate.ID, UserID: candidate.UserID, TokenHash: candidate.TokenHash, ExpiresAt: candidate.ExpiresAt}, ok, err
}

func (s *Server) confirmRecovery(w http.ResponseWriter, r *http.Request) {
	var req recoveryConfirmRequest
	if !decode(w, r, &req) {
		return
	}
	req.Email = strings.TrimSpace(req.Email)
	if len(req.Email) > 320 || len(req.Code) > 64 || len(req.NewPassword) > 256 {
		writeError(w, http.StatusUnauthorized, "invalid or expired recovery code")
		return
	}
	if req.Purpose != "reset_password" && req.Purpose != "bypass_2fa" {
		writeError(w, http.StatusBadRequest, "invalid recovery purpose")
		return
	}
	if req.Purpose == "reset_password" {
		if err := validatePassword(req.NewPassword); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if len(strings.TrimSpace(req.Code)) < 6 {
		writeError(w, http.StatusUnauthorized, "invalid or expired recovery code")
		return
	}
	attempted := make(map[int64]struct{})
	for range 5 {
		candidate, ok, err := s.takeRecoveryCandidate(r.Context(), req.Email, req.Purpose, attempted)
		if err != nil {
			serverError(w, err)
			return
		}
		if !ok {
			break
		}
		attempted[candidate.ID] = struct{}{}
		if !auth.Check(candidate.TokenHash, strings.TrimSpace(req.Code)) {
			continue
		}
		var passwordHash string
		if req.Purpose == "reset_password" {
			passwordHash, err = auth.Hash(req.NewPassword)
			if err != nil {
				serverError(w, err)
				return
			}
		}
		redeemed, err := s.Store.RedeemRecoveryToken(r.Context(), candidate.ID, candidate.UserID, passwordHash, time.Now().UTC())
		if err != nil {
			serverError(w, err)
			return
		}
		if redeemed.DisabledUser {
			writeError(w, http.StatusUnauthorized, "invalid or expired recovery code")
			return
		}
		if !redeemed.Redeemed {
			continue
		}
		if req.Purpose == "reset_password" {
			s.Sessions.DeleteUser(candidate.UserID)
			_ = s.Store.Audit(r.Context(), candidate.UserID, "password_recovered", "mailbox OTP")
			writeJSON(w, http.StatusOK, map[string]string{"status": "password_reset"})
			return
		}
		u, err := s.Store.UserByID(r.Context(), candidate.UserID)
		if err != nil || !u.Enabled {
			writeError(w, http.StatusUnauthorized, "invalid or expired recovery code")
			return
		}
		s.issueSession(w, r, candidate.UserID)
		_ = s.Store.Audit(r.Context(), candidate.UserID, "two_factor_bypassed", "mailbox OTP")
		writeJSON(w, http.StatusOK, map[string]any{"status": "signed_in", "user": publicUser(u)})
		return
	}
	writeError(w, http.StatusUnauthorized, "invalid or expired recovery code")
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
	if err := validatePassword(req.New); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	hash, err := auth.Hash(req.New)
	if err != nil {
		serverError(w, err)
		return
	}
	if err := s.Store.UpdatePasswordHash(r.Context(), u.ID, hash); err != nil {
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
	if err := s.Store.UpdatePINHash(r.Context(), u.ID, hash); err != nil {
		serverError(w, err)
		return
	}
	if s.Calls != nil {
		s.Calls.InvalidateUserSessions(u.ID)
	}
	_ = s.Store.Audit(r.Context(), u.ID, "pin_changed", "")
	writeJSON(w, http.StatusOK, map[string]string{"status": "changed"})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, false)
	if ok {
		// The browser can restore an in-memory session after a page reload. It
		// needs the matching CSRF token before it can make any state-changing
		// request, so expose the token only for the already-authenticated
		// session represented by this request.
		if session, exists := s.Sessions.Get(sessionToken(r)); exists && session.UserID == u.ID {
			w.Header().Set("X-CSRF-Token", session.CSRF)
		}
		writeJSON(w, http.StatusOK, publicUser(u))
	}
}
