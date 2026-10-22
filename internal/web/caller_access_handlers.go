package web

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/voxmail/voxmail/internal/phone"
	"github.com/voxmail/voxmail/internal/store"
)

// verificationSettings exposes the administrator verification policy.
func (s *Server) verificationSettings(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, false)
	if !ok {
		return
	}
	if u.Role != "admin" {
		writeError(w, http.StatusForbidden, "administrator access required")
		return
	}
	settings, err := s.Store.VerificationSettings(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (s *Server) saveVerificationSettings(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, true)
	if !ok {
		return
	}
	if u.Role != "admin" {
		writeError(w, http.StatusForbidden, "administrator access required")
		return
	}
	var req store.VerificationSettings
	if !decode(w, r, &req) {
		return
	}
	if req.DialIn != "" {
		req.DialIn = phone.Normalize(req.DialIn)
	}
	if err := s.Store.SaveVerificationSettings(r.Context(), req); err != nil {
		serverError(w, err)
		return
	}
	_ = s.Store.Audit(r.Context(), u.ID, "verification_settings_changed", "")
	writeJSON(w, http.StatusOK, req)
}

// adminAddWhitelist places a number on any user's caller list without a
// verification call, bound to one of that user's accounts.
func (s *Server) adminAddWhitelist(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, true)
	if !ok {
		return
	}
	if u.Role != "admin" {
		writeError(w, http.StatusForbidden, "administrator access required")
		return
	}
	var req struct {
		UserID    string `json:"user_id"`
		Phone     string `json:"phone"`
		AccountID string `json:"account_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	req.Phone = phone.Normalize(req.Phone)
	if req.UserID == "" || req.Phone == "" || req.AccountID == "" {
		writeError(w, http.StatusBadRequest, "user, phone, and account are required")
		return
	}
	if err := s.Store.AddCallerAccount(r.Context(), req.UserID, req.Phone, req.AccountID, true); err != nil {
		if errors.Is(err, store.ErrAccountNotFound) {
			writeError(w, http.StatusBadRequest, "unknown account")
			return
		}
		if strings.Contains(err.Error(), "belongs to another user") {
			writeError(w, http.StatusBadRequest, "account belongs to another user")
			return
		}
		serverError(w, err)
		return
	}
	_ = s.Store.Audit(r.Context(), u.ID, "admin_whitelist_added", req.UserID+" "+req.Phone)
	writeJSON(w, http.StatusCreated, map[string]string{"user_id": req.UserID, "phone": req.Phone, "account_id": req.AccountID})
}

// reorderWhitelist is the administrator's account-menu ordering for one
// number. Users cannot reorder the caller menu.
func (s *Server) reorderWhitelist(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, true)
	if !ok {
		return
	}
	if u.Role != "admin" {
		writeError(w, http.StatusForbidden, "administrator access required")
		return
	}
	var req struct {
		UserID     string   `json:"user_id"`
		Phone      string   `json:"phone"`
		AccountIDs []string `json:"account_ids"`
	}
	if !decode(w, r, &req) {
		return
	}
	req.Phone = phone.Normalize(req.Phone)
	if req.UserID == "" || req.Phone == "" {
		writeError(w, http.StatusBadRequest, "user and phone are required")
		return
	}
	if err := s.Store.ReorderCallerAccounts(r.Context(), req.UserID, req.Phone, req.AccountIDs); err != nil {
		serverError(w, err)
		return
	}
	_ = s.Store.Audit(r.Context(), u.ID, "admin_whitelist_reordered", req.UserID+" "+req.Phone)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// addWhitelist starts a number add. The number is stored unverified and a
// verification code is issued by the direction the administrator policy
// allows; when both directions are open the request picks one.
func (s *Server) addWhitelist(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, true)
	if !ok {
		return
	}
	var req struct {
		Phone     string `json:"phone"`
		AccountID string `json:"account_id"`
		Method    string `json:"method"`
	}
	if !decode(w, r, &req) {
		return
	}
	req.Phone = phone.Normalize(req.Phone)
	if req.Phone == "" {
		writeError(w, http.StatusBadRequest, "phone is required")
		return
	}
	settings, err := s.Store.VerificationSettings(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	if !settings.Incoming && !settings.Outgoing {
		writeError(w, http.StatusConflict, "number verification is disabled")
		return
	}
	method := strings.ToLower(strings.TrimSpace(req.Method))
	if !settings.Incoming && !settings.Outgoing {
		// Unreachable; kept for symmetry with the policy read above.
		writeError(w, http.StatusConflict, "number verification is disabled")
		return
	}
	if settings.Incoming && settings.Outgoing {
		if method != "incoming" && method != "outgoing" {
			writeError(w, http.StatusBadRequest, "choose a verification method: incoming or outgoing")
			return
		}
	} else if settings.Incoming {
		method = "incoming"
	} else {
		method = "outgoing"
	}
	if req.AccountID == "" {
		writeError(w, http.StatusBadRequest, "account is required")
		return
	}
	accounts, err := s.Store.ListAccounts(r.Context(), u.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	owned := false
	for _, account := range accounts {
		if account.ID == req.AccountID {
			owned = true
			break
		}
	}
	if !owned {
		writeError(w, http.StatusBadRequest, "unknown account")
		return
	}
	code, err := newVerificationCode()
	if err != nil {
		serverError(w, err)
		return
	}
	if err := s.Store.CreateCallerVerification(r.Context(), u.ID, req.Phone, method, code, verificationTTL); err != nil {
		serverError(w, err)
		return
	}
	if err := s.Store.AddWhitelist(r.Context(), store.WhitelistEntry{UserID: u.ID, Phone: req.Phone}); err != nil {
		serverError(w, err)
		return
	}
	if method == "outgoing" {
		if s.Verification == nil {
			writeError(w, http.StatusServiceUnavailable, "the call service is not running")
			return
		}
		domain := s.verificationDomain(r.Context())
		if domain == "" {
			writeError(w, http.StatusConflict, "an SIP registrar/domain must be configured to place verification calls")
			return
		}
		uri := "sip:" + req.Phone + "@" + domain
		if strings.HasPrefix(strings.ToLower(req.Phone), "sip:") {
			uri = req.Phone
		}
		if err := s.Verification.DialVerification(r.Context(), uri, u.ID, code); err != nil {
			writeError(w, http.StatusServiceUnavailable, "the verification call could not be placed")
			return
		}
		_ = s.Store.Audit(r.Context(), u.ID, "caller_whitelist_verification_calling", req.Phone)
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "calling", "method": method, "phone": req.Phone})
		return
	}
	_ = s.Store.Audit(r.Context(), u.ID, "caller_whitelist_verification_pending", req.Phone)
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "call_in", "method": method, "phone": req.Phone, "dial_in": s.verificationDialIn(r.Context(), settings.DialIn)})
}

// verifyWhitelist consumes a submitted code, marks the number verified, and
// binds it to the account the user picked when starting the add.
func (s *Server) verifyWhitelist(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, true)
	if !ok {
		return
	}
	var req struct {
		Phone     string `json:"phone"`
		Code      string `json:"code"`
		AccountID string `json:"account_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	req.Phone = phone.Normalize(req.Phone)
	if req.Phone == "" || strings.TrimSpace(req.Code) == "" {
		writeError(w, http.StatusBadRequest, "phone and code are required")
		return
	}
	if err := s.Store.ConsumeCallerVerification(r.Context(), u.ID, req.Phone, req.Code); err != nil {
		if errors.Is(err, store.ErrVerificationInvalid) {
			writeError(w, http.StatusBadRequest, "that code is invalid or expired")
			return
		}
		serverError(w, err)
		return
	}
	if req.AccountID != "" {
		if err := s.Store.AddCallerAccount(r.Context(), u.ID, req.Phone, req.AccountID, true); err != nil {
			if errors.Is(err, store.ErrAccountNotFound) || strings.Contains(err.Error(), "belongs to another user") {
				writeError(w, http.StatusBadRequest, "unknown account")
				return
			}
			serverError(w, err)
			return
		}
	}
	_ = s.Store.Audit(r.Context(), u.ID, "caller_whitelist_verified", req.Phone)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// verificationDialIn resolves the call-in number shown to users: the
// administrator-entered DID wins; otherwise the SIP domain is offered as the
// call-in address when one is configured.
func (s *Server) verificationDialIn(ctx context.Context, adminDialIn string) string {
	if adminDialIn != "" {
		return adminDialIn
	}
	settings, err := s.Store.GetSIP(ctx)
	if err != nil || !settings.Enabled {
		return ""
	}
	return strings.TrimSpace(settings.Domain)
}

func (s *Server) verificationDomain(ctx context.Context) string {
	settings, err := s.Store.GetSIP(ctx)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(settings.Domain)
}

// verificationTTL is how long a verification code stays usable.
const verificationTTL = 10 * time.Minute

// newVerificationCode returns a six digit proof code.
func newVerificationCode() (string, error) {
	for attempt := 0; attempt < 8; attempt++ {
		var raw [4]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return "", err
		}
		n := int(binary.BigEndian.Uint32(raw[:]) % 1000000)
		if n >= 100000 {
			return fmt.Sprintf("%06d", n), nil
		}
	}
	return "913247", nil
}

// adminWhitelistList returns one user's caller list with the accounts each
// number can reach, plus that user's accounts for the admin's add form. Only
// administrators may call it.
func (s *Server) adminWhitelistList(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, false)
	if !ok {
		return
	}
	if u.Role != "admin" {
		writeError(w, http.StatusForbidden, "administrator access required")
		return
	}
	userID := r.URL.Query().Get("user_id")
	if userID == "" {
		writeError(w, http.StatusBadRequest, "user is required")
		return
	}
	accounts, err := s.Store.ListAccounts(r.Context(), userID)
	if err != nil {
		serverError(w, err)
		return
	}
	rows, err := s.Store.ListWhitelistDetail(r.Context(), userID)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"whitelist": rows, "accounts": accounts})
}
