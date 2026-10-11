package web

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/skip2/go-qrcode"
	"github.com/voxmail/voxmail/internal/auth"
)

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
		writeFieldError(w, "current_password", "current password is incorrect")
		return
	}
	secretValue, err := auth.GenerateTOTPSecret()
	if err != nil {
		serverError(w, err)
		return
	}
	label := url.PathEscape("VOXMail:" + u.Username)
	issuer := url.QueryEscape("VOXMail")
	uri := "otpauth://totp/" + label + "?secret=" + secretValue + "&issuer=" + issuer
	// Render locally: an external QR service would disclose the TOTP secret.
	png, err := qrcode.Encode(uri, qrcode.Medium, 256)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"secret": secretValue, "provisioning_uri": uri, "qr_data_uri": "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)})
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
	if !auth.Check(u.PasswordHash, req.CurrentPassword) {
		writeFieldError(w, "current_password", "current password is incorrect")
		return
	}
	if !auth.TOTP(req.Secret, req.Code, time.Now().UTC()) {
		writeFieldError(w, "code", "authenticator code is incorrect")
		return
	}
	if s.Secrets == nil {
		serverError(w, nil)
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
	if err := s.Store.EnableTOTP(r.Context(), u.ID, sealed, hashes, time.Now().UTC()); err != nil {
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
	if !auth.Check(u.PasswordHash, req.CurrentPassword) {
		writeFieldError(w, "current_password", "current password is incorrect")
		return
	}
	if u.TOTPSecret == "" || !auth.TOTP(s.openTOTPSecret(u.TOTPSecret), req.Code, time.Now().UTC()) {
		writeFieldError(w, "code", "authenticator code is incorrect")
		return
	}
	if err := s.Store.DisableTOTP(r.Context(), u.ID); err != nil {
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

// consumeTOTPReplay atomically accepts a strictly newer clock step. The
// verifier still allows one step of clock skew, but a code for any accepted
// step can only authenticate once for this user.
func (s *Server) consumeTOTPReplay(ctx context.Context, userID string, step int64) bool {
	ok, err := s.Store.ConsumeTOTPReplay(ctx, userID, step)
	return err == nil && ok
}

func (s *Server) consumeBackupCode(ctx context.Context, userID, candidate string) bool {
	ok, err := s.Store.ConsumeBackupCode(ctx, userID, candidate, auth.Check, time.Now().UTC())
	if err != nil || !ok {
		return false
	}
	_ = s.Store.Audit(ctx, userID, "backup_code_used", "")
	return true
}
