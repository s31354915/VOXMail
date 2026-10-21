package secret

import (
	"crypto/cipher"
	"encoding/base64"
	"strings"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	box, err := New("correct horse battery staple")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	plain := "s3cr3t-imap-password"
	sealed, err := box.Seal(plain)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if sealed == plain {
		t.Fatal("Seal returned plaintext")
	}
	opened, err := box.Open(sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if opened != plain {
		t.Fatalf("Open mismatch: got %q want %q", opened, plain)
	}
}

func TestOpenTampered(t *testing.T) {
	box, err := New("correct horse battery staple")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sealed, err := box.Seal("do not modify")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	sealed = sealed[:len(sealed)-2] + "xx" + sealed[len(sealed)-2:]
	if _, err := box.Open(sealed); err == nil {
		t.Fatal("Open accepted tampered ciphertext")
	}
}

func TestWrongKey(t *testing.T) {
	box, err := New("key one")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sealed, err := box.Seal("value")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	other, err := New("key two")
	if err != nil {
		t.Fatalf("New other: %v", err)
	}
	if _, err := other.Open(sealed); err == nil {
		t.Fatal("Open succeeded with the wrong key")
	}
}

func TestLegacyCompatibility(t *testing.T) {
	key := "legacy deployment key"
	block, err := legacyBlock(key)
	if err != nil {
		t.Fatalf("legacyBlock: %v", err)
	}
	legacyAEAD, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("NewGCM: %v", err)
	}
	value := "stored by an older version"
	nonce := make([]byte, legacyAEAD.NonceSize())
	nonce[0] = 1
	buf := legacyAEAD.Seal(nonce, nonce, []byte(value), nil)
	oldEncoding := base64.RawStdEncoding.EncodeToString(buf)
	box, err := New(key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	opened, err := box.Open(oldEncoding)
	if err != nil {
		t.Fatalf("Open legacy: %v", err)
	}
	if opened != value {
		t.Fatalf("Legacy mismatch: got %q want %q", opened, value)
	}
	if !strings.HasPrefix(opened, "stored by") {
		t.Fatalf("unexpected value %q", opened)
	}
}

func TestSealUsesV2Envelope(t *testing.T) {
	box, err := New("correct horse battery staple")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sealed, err := box.Seal("value")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	data, err := base64.RawStdEncoding.DecodeString(sealed)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(data) == 0 || data[0] != envelopeV2 {
		t.Fatalf("sealed record missing v2 envelope marker: %x", data[:1])
	}
}

// Records sealed by the HKDF-only schedule (no marker) must keep opening.
func TestOpenV1Record(t *testing.T) {
	key := "hkdf-era deployment key"
	box, err := New(key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	value := "stored by the v1 schedule"
	nonce := make([]byte, box.v1AEAD.NonceSize())
	nonce[0] = 1
	buf := box.v1AEAD.Seal(nonce, nonce, []byte(value), nil)
	encoded := base64.RawStdEncoding.EncodeToString(buf)
	opened, err := box.Open(encoded)
	if err != nil {
		t.Fatalf("Open v1: %v", err)
	}
	if opened != value {
		t.Fatalf("v1 mismatch: got %q want %q", opened, value)
	}
}

// A legacy record whose first nonce byte equals the v2 envelope marker must
// still open through the fallbacks after the failed v2 attempt.
func TestOpenLegacyRecordWithEnvelopeMarkerCollision(t *testing.T) {
	key := "legacy deployment key"
	block, err := legacyBlock(key)
	if err != nil {
		t.Fatalf("legacyBlock: %v", err)
	}
	legacyAEAD, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("NewGCM: %v", err)
	}
	value := "stored by an older version"
	nonce := make([]byte, legacyAEAD.NonceSize())
	nonce[0] = envelopeV2
	buf := legacyAEAD.Seal(nonce, nonce, []byte(value), nil)
	encoded := base64.RawStdEncoding.EncodeToString(buf)
	box, err := New(key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	opened, err := box.Open(encoded)
	if err != nil {
		t.Fatalf("Open legacy: %v", err)
	}
	if opened != value {
		t.Fatalf("Legacy mismatch: got %q want %q", opened, value)
	}
}

// A v2 record cannot be opened with a different deployment key.
func TestWrongKeyV2(t *testing.T) {
	box, err := New("key one")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sealed, err := box.Seal("value")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	other, err := New("key two")
	if err != nil {
		t.Fatalf("New other: %v", err)
	}
	if _, err := other.Open(sealed); err == nil {
		t.Fatal("Open succeeded with the wrong key")
	}
}
