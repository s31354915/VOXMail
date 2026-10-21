package auth

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPasswordHash(t *testing.T) {
	hash, err := Hash("correct horse")
	if err != nil || !Check(hash, "correct horse") || Check(hash, "wrong") {
		t.Fatal("password hash verification failed")
	}
	if _, err := Hash(string(make([]byte, MaxPasswordBytes+1))); !errors.Is(err, ErrPasswordTooLong) {
		t.Fatalf("long password error=%v, want ErrPasswordTooLong", err)
	}
	if !strings.HasPrefix(hash, "$2a$12$") && !strings.HasPrefix(hash, "$2y$12$") {
		t.Fatalf("password hash cost %q, want cost 12", hash[:7])
	}
}

func TestTOTP(t *testing.T) {
	step, ok := TOTPWithStep("GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ", "287082", time.Unix(59, 0))
	if !ok || step != 1 || !TOTP("JBSWY3DPEHPK3PXP", "996554", time.Unix(59, 0)) {
		t.Fatal("known TOTP vector failed")
	}
	for _, now := range []time.Time{time.Unix(0, 0), time.Unix(30, 0), time.Unix(60, 0), time.Unix(89, 0)} {
		if !TOTP("GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ", "287082", now) {
			t.Fatalf("TOTP one-step skew rejected at %v", now)
		}
	}
	if TOTP("GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ", "287082", time.Unix(90, 0)) {
		t.Fatal("TOTP code accepted outside the one-step skew window")
	}
	if _, ok := TOTPWithStep("not-base32", "282760", time.Unix(59, 0)); ok {
		t.Fatal("malformed TOTP secret was accepted")
	}
}
