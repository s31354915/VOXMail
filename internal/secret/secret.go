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
	"sync"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/hkdf"
)

// envelopeV2 marks records sealed under the argon2id-stretched key schedule.
// Older records carry no marker: their first byte is the GCM nonce, so a
// legacy record whose nonce happens to start with this marker still opens
// through the v1 and legacy fallbacks after the failed v2 attempt.
const envelopeV2 byte = 0x02

// Argon2id parameters for the v2 key schedule. Memory is kept at 64 MiB with
// a single pass so startup and sealing stay affordable on small deployments
// while every brute-force guess still costs a memory-hard run.
const (
	argon2Time    uint32 = 1
	argon2Memory  uint32 = 64 * 1024
	argon2Threads uint8  = 1
	argon2KeyLen  uint32 = 32
)

// Box encrypts provider credentials and recovery material using AES-GCM.
// Sealing uses the v2 schedule (argon2id + HKDF-SHA256); opening transparently
// handles v2, v1 (HKDF only), and legacy (raw SHA-256) records.
type Box struct {
	key        string
	salt       []byte
	aead       cipher.AEAD
	v1AEAD     cipher.AEAD
	legacyAEAD cipher.AEAD
	v2Once     sync.Once
	v2Err      error
}

func New(key string) (*Box, error) {
	salt := []byte("voxmail")
	block, err := aes.NewCipher(hkdfKey([]byte(key), salt, []byte("voxmail:key")))
	if err != nil {
		return nil, err
	}
	v1AEAD, err := cipher.NewGCM(block)
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
	return &Box{key: key, salt: salt, v1AEAD: v1AEAD, legacyAEAD: legacyAEAD}, nil
}

// v2AEAD derives the argon2id-stretched AEAD on first use. Sealing a record
// or opening a v2-prefixed record pays the stretch; deployments that only
// open older records never do.
func (b *Box) v2AEAD() (cipher.AEAD, error) {
	b.v2Once.Do(func() {
		stretched := argon2.IDKey([]byte(b.key), b.salt, argon2Time, argon2Memory, argon2Threads, argon2KeyLen)
		block, err := aes.NewCipher(hkdfKey(stretched, b.salt, []byte("voxmail:key:v2")))
		if err != nil {
			b.v2Err = err
			return
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			b.v2Err = err
			return
		}
		b.aead = aead
	})
	return b.aead, b.v2Err
}

// hkdfKey derives 32 bytes of key material with HKDF-SHA256. The salt is
// static because the schedule must stay deterministic without stored state;
// the v2 schedule's argon2id stretch is what raises the cost of guessing a
// weak passphrase.
func hkdfKey(ikm, salt, info []byte) []byte {
	derived := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, ikm, salt, info), derived); err != nil {
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
	aead, err := b.v2AEAD()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	data := aead.Seal(nonce, nonce, []byte(value), nil)
	return base64.RawStdEncoding.EncodeToString(append([]byte{envelopeV2}, data...)), nil
}

// Open decrypts a sealed record, trying the v2, v1, and legacy schedules.
// A v2 envelope is marked with envelopeV2; older records are identified by
// AEAD authentication, not by a marker, so the fallbacks run on any record
// the marked attempt does not open.
func (b *Box) Open(encoded string) (string, error) {
	data, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("decode secret: %w", err)
	}
	var firstErr error
	if len(data) > 0 && data[0] == envelopeV2 {
		aead, aeadErr := b.v2AEAD()
		if aeadErr != nil {
			return "", fmt.Errorf("open secret: %w", aeadErr)
		}
		if plain, ok := tryOpen(aead, data[1:]); ok {
			return plain, nil
		} else {
			firstErr = fmt.Errorf("open secret: v2 record failed authentication")
		}
	}
	if plain, ok := tryOpen(b.v1AEAD, data); ok {
		return plain, nil
	} else if firstErr == nil {
		firstErr = fmt.Errorf("open secret: v1 record failed authentication")
	}
	if plain, ok := tryOpen(b.legacyAEAD, data); ok {
		return plain, nil
	}
	if firstErr == nil {
		firstErr = errors.New("open secret: record failed authentication")
	}
	return "", firstErr
}

// tryOpen decrypts one AEAD blob, reporting success instead of an error so
// the schedule fallbacks can run in sequence without wrapping errors.
func tryOpen(aead cipher.AEAD, data []byte) (string, bool) {
	if aead == nil {
		return "", false
	}
	n := aead.NonceSize()
	if len(data) < n {
		return "", false
	}
	plain, err := aead.Open(nil, data[:n], data[n:], nil)
	if err != nil {
		return "", false
	}
	return string(plain), true
}
