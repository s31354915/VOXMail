package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const MaxPasswordBytes = 72

var ErrPasswordTooLong = errors.New("password exceeds bcrypt's 72-byte limit")

func Hash(value string) (string, error) {
	if len([]byte(value)) > MaxPasswordBytes {
		return "", ErrPasswordTooLong
	}
	b, err := bcrypt.GenerateFromPassword([]byte(value), bcrypt.DefaultCost)
	return string(b), err
}

func Check(encoded, value string) bool {
	return bcrypt.CompareHashAndPassword([]byte(encoded), []byte(value)) == nil
}

func RandomToken(size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b), nil
}

func TOTP(secret, code string, now time.Time) bool {
	_, ok := TOTPWithStep(secret, code, now)
	return ok
}

// TOTPWithStep validates a code in the same three-step clock-skew window as
// TOTP and returns the accepted Unix time-step. Callers that need replay
// resistance can persist that step atomically per account.
func TOTPWithStep(secret, code string, now time.Time) (int64, bool) {
	code = strings.TrimSpace(code)
	if len(code) != 6 {
		return 0, false
	}
	step := now.Unix() / 30
	for offset := int64(-1); offset <= 1; offset++ {
		candidate := step + offset
		if candidate < 0 {
			continue
		}
		if generateTOTPAtStep(secret, candidate) == code {
			return candidate, true
		}
	}
	return 0, false
}

func GenerateTOTPSecret() (string, error) {
	return RandomToken(20)
}

func generateTOTP(secret string, now time.Time) string {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil {
		return ""
	}
	return generateTOTPAtStepWithKey(key, now.Unix()/30)
}

func generateTOTPAtStep(secret string, step int64) string {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil || step < 0 {
		return ""
	}
	return generateTOTPAtStepWithKey(key, step)
}

func generateTOTPAtStepWithKey(key []byte, step int64) string {
	message := make([]byte, 8)
	binary.BigEndian.PutUint64(message, uint64(step))
	h := hmac.New(sha1.New, key)
	_, _ = h.Write(message)
	sum := h.Sum(nil)
	offset := sum[len(sum)-1] & 15
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", value%1000000)
}
