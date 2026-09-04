package auth

import (
	"testing"
	"time"
)

func TestPasswordHash(t *testing.T) {
	hash, err := Hash("correct horse")
	if err != nil || !Check(hash, "correct horse") || Check(hash, "wrong") {
		t.Fatal("password hash verification failed")
	}
}

func TestTOTP(t *testing.T) {
	if !TOTP("JBSWY3DPEHPK3PXP", "282760", time.Unix(59, 0)) {
		t.Fatal("known TOTP vector failed")
	}
}
