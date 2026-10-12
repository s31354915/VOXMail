package web

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/makiuchi-d/gozxing"
	qrreader "github.com/makiuchi-d/gozxing/qrcode"
	"github.com/voxmail/voxmail/internal/auth"
	"github.com/voxmail/voxmail/internal/secret"
	"github.com/voxmail/voxmail/internal/store"
)

func TestTwoFactorQRProvisioningAndVerification(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/qr.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	box, err := secret.New("disposable-qr-test-key-01234567890123456789")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: db, Secrets: box}
	h := s.Handler()
	csrf, cookies := issue(t, h)
	// Special characters must survive the URI path, including spaces and '+'.
	username := "admin QR + label"
	if _, err := db.DB.Exec(`UPDATE users SET username=? WHERE username='admin'`, username); err != nil {
		t.Fatal(err)
	}
	post := func(path string, body any, authenticated bool) *httptest.ResponseRecorder {
		t.Helper()
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(data))
		r.Header.Set("Content-Type", "application/json")
		if authenticated {
			r.Header.Set("X-CSRF-Token", csrf)
			for _, c := range cookies {
				r.AddCookie(c)
			}
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	path := "/api/v1/security/2fa/setup"
	if w := post(path, map[string]string{"current_password": "a-strong-password"}, false); w.Code != 401 {
		t.Fatalf("unauthenticated setup status=%d", w.Code)
	}
	if w := post(path, map[string]string{"current_password": "wrong"}, true); w.Code != 400 || strings.Contains(w.Body.String(), "qr_data_uri") {
		t.Fatalf("invalid password response=%d %s", w.Code, w.Body.String())
	}
	w := post(path, map[string]string{"current_password": "a-strong-password"}, true)
	if w.Code != 200 {
		t.Fatalf("setup status=%d %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("provisioning response must not be cached")
	}
	var setup struct {
		Secret string `json:"secret"`
		URI    string `json:"provisioning_uri"`
		QR     string `json:"qr_data_uri"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &setup); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(setup.QR, "data:image/png;base64,") {
		t.Fatal("QR is not a local PNG data URI")
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(setup.QR, "data:image/png;base64,"))
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() != 512 || img.Bounds().Dy() != 512 {
		t.Fatalf("QR dimensions=%v, want 512x512", img.Bounds())
	}
	bitmap, err := gozxing.NewBinaryBitmapFromImage(img)
	if err != nil {
		t.Fatal(err)
	}
	scanned, err := qrreader.NewQRCodeReader().Decode(bitmap, nil)
	if err != nil {
		t.Fatalf("authenticator QR cannot be scanned: %v", err)
	}
	if scanned.GetText() != setup.URI {
		t.Fatal("scanned QR differs from provisioning URI")
	}
	uri, err := url.Parse(scanned.GetText())
	if err != nil {
		t.Fatal(err)
	}
	if uri.Scheme != "otpauth" || uri.Host != "totp" || uri.Path != "/VOXMail:"+username || uri.Query().Get("issuer") != "VOXMail" || uri.Query().Get("secret") != setup.Secret {
		t.Fatal("QR has incorrect authenticator account, issuer, or secret")
	}
	u, err := db.UserByUsername(context.Background(), username)
	if err != nil {
		t.Fatal(err)
	}
	if u.TOTPSecret != "" {
		t.Fatal("generating a QR must not enable TOTP before verification")
	}
	badCode := "000000"
	for auth.TOTP(setup.Secret, badCode, time.Now()) {
		badCode = "111111"
	}
	if w := post("/api/v1/security/2fa/enable", twoFARequest{CurrentPassword: "a-strong-password", Secret: setup.Secret, Code: badCode}, true); w.Code != 400 {
		t.Fatalf("invalid verification status=%d", w.Code)
	}
	u, err = db.UserByUsername(context.Background(), username)
	if err != nil || u.TOTPSecret != "" {
		t.Fatal("failed verification changed authenticator state")
	}
	// Calculate a code independently from the verifier used by the endpoint.
	code := authenticatorTestCode(t, setup.Secret, time.Now())
	w = post("/api/v1/security/2fa/enable", twoFARequest{CurrentPassword: "a-strong-password", Secret: setup.Secret, Code: code}, true)
	if w.Code != 200 {
		t.Fatalf("verified enable status=%d %s", w.Code, w.Body.String())
	}
	var result struct {
		Codes []string `json:"backup_codes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Codes) != 10 {
		t.Fatalf("backup code count=%d", len(result.Codes))
	}
	u, err = db.UserByUsername(context.Background(), username)
	if err != nil || u.TOTPSecret == "" || u.TOTPSecret == setup.Secret {
		t.Fatal("enabled secret was not sealed")
	}
	opened, err := box.Open(u.TOTPSecret)
	if err != nil || opened != setup.Secret {
		t.Fatal("scanned secret does not match stored authenticator")
	}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	for _, c := range cookies {
		r.AddCookie(c)
	}
	me := httptest.NewRecorder()
	h.ServeHTTP(me, r)
	if me.Code != 200 || strings.Contains(me.Body.String(), setup.Secret) || strings.Contains(me.Body.String(), result.Codes[0]) {
		t.Fatal("current-user response exposes recovery material or lost session after validation failure")
	}
}

func authenticatorTestCode(t *testing.T, secretValue string, now time.Time) string {
	t.Helper()
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secretValue)
	if err != nil {
		t.Fatal(err)
	}
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(now.Unix()/30))
	mac := hmac.New(sha1.New, key)
	mac.Write(counter[:])
	digest := mac.Sum(nil)
	offset := digest[len(digest)-1] & 15
	return fmt.Sprintf("%06d", (binary.BigEndian.Uint32(digest[offset:offset+4])&0x7fffffff)%1000000)
}
