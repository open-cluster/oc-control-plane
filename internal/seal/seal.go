// Package seal encrypts credentials that must later be presented to a provider.
package seal

import (
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"fmt"
)

// KeyLength is the required AES-256 key size in bytes.
const KeyLength = 32

// ErrNoKey reports use of a zero Sealer or construction with a key of the wrong size.
var ErrNoKey = errors.New("no sealing key is configured")

// Sealer encrypts credentials with random-nonce AES-GCM.
type Sealer struct {
	aead cipher.AEAD
}

// New constructs a Sealer from exactly one AES-256 key.
func New(key []byte) (Sealer, error) {
	if len(key) != KeyLength {
		return Sealer{}, fmt.Errorf("%w: key must be exactly %d bytes", ErrNoKey, KeyLength)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return Sealer{}, fmt.Errorf("seal: building cipher: %w", err)
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return Sealer{}, fmt.Errorf("seal: building GCM: %w", err)
	}
	return Sealer{aead: aead}, nil
}

// Configured reports whether the deployment supplied a valid key.
func (s Sealer) Configured() bool { return s.aead != nil }

// Seal encrypts plaintext and authenticates its owning record as additional data.
func (s Sealer) Seal(plaintext string, aad []byte) ([]byte, error) {
	if !s.Configured() {
		return nil, ErrNoKey
	}
	return s.aead.Seal(nil, nil, []byte(plaintext), aad), nil
}

// Open authenticates and decrypts nonce-prefixed ciphertext for its owning record.
func (s Sealer) Open(ciphertext []byte, aad []byte) (string, error) {
	if !s.Configured() {
		return "", ErrNoKey
	}
	plaintext, err := s.aead.Open(nil, nil, ciphertext, aad)
	if err != nil {
		return "", errors.New("seal: the stored secret could not be opened")
	}
	return string(plaintext), nil
}
