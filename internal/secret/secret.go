package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

// Box encrypts provider credentials and recovery material using AES-GCM.
type Box struct {
	aead       cipher.AEAD
	legacyAEAD cipher.AEAD
}

func New(key string) (*Box, error) {
	block, err := aes.NewCipher(deriveKey(key, []byte("voxmail")))
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	legacy, err := legacyBlock(key)
	if err != nil {
		return nil, err
	}
	legacyAEAD, err := cipher.NewGCM(legacy)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead, legacyAEAD: legacyAEAD}, nil
}

// deriveKey stretches the deployment key with HKDF-SHA256 so short or
// low-entropy passphrases never map directly onto the AES-256 key material.
func deriveKey(key string, salt []byte) []byte {
	derived := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, []byte(key), salt, []byte("voxmail:key")), derived); err != nil {
		panic(err)
	}
	return derived
}

// legacyBlock reproduces the pre-HKDF key schedule so records sealed by older
// versions can still be opened after an upgrade.
func legacyBlock(key string) (cipher.Block, error) {
	hash := sha256.Sum256([]byte(key))
	return aes.NewCipher(hash[:])
}

func (b *Box) Seal(value string) (string, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	data := b.aead.Seal(nonce, nonce, []byte(value), nil)
	return base64.RawStdEncoding.EncodeToString(data), nil
}

func (b *Box) Open(encoded string) (string, error) {
	data, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("decode secret: %w", err)
	}
	n := b.aead.NonceSize()
	if len(data) < n {
		return "", errors.New("secret is truncated")
	}
	plain, err := b.aead.Open(nil, data[:n], data[n:], nil)
	if err == nil {
		return string(plain), nil
	}
	plain, legacyErr := b.legacyAEAD.Open(nil, data[:n], data[n:], nil)
	if legacyErr != nil {
		return "", fmt.Errorf("open secret: %w", err)
	}
	return string(plain), nil
}
