package seal

import (
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"fmt"
)

const KeyLength = 32

var ErrNoKey = errors.New("no sealing key is configured")

type Sealer struct {
	aead cipher.AEAD
}

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

func (s Sealer) Configured() bool { return s.aead != nil }

func (s Sealer) Seal(plaintext string, aad []byte) ([]byte, error) {
	if !s.Configured() {
		return nil, ErrNoKey
	}
	// Additional data binds ciphertext to its owning record without storing that data twice.
	return s.aead.Seal(nil, nil, []byte(plaintext), aad), nil
}

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
