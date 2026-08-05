package identity

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

// SealKeyLength is the key size this build uses. AES-256 rather than AES-128 because the key
// is configuration read from a file, so the larger one costs nothing anybody notices.
const SealKeyLength = 32

// ErrNoSealKey reports a deployment asked to hold a provider's client secret with no key
// configured to seal it under. It is a refusal to start rather than a fallback to storing the
// secret in the clear.
var ErrNoSealKey = errors.New("no identity encryption key is configured")

// Sealer holds the key a provider's client secret is stored under.
//
// The client secret is the one credential in this schema that is ENCRYPTED rather than
// digested, and the reason is worth stating: every other credential here is only ever compared
// against, so a one-way digest suffices. This one has to be PRESENTED to the identity
// provider's token endpoint, so the process must be able to read it back.
//
// That makes the key the thing that matters. It is read from a file the deployment names, in
// the same shape a placement's DSN is, and never from an environment value.
type Sealer struct {
	block cipher.AEAD
}

// NewSealer builds a sealer from the configured key.
func NewSealer(key []byte) (Sealer, error) {
	if len(key) != SealKeyLength {
		return Sealer{}, fmt.Errorf("%w: the key must be exactly %d bytes",
			ErrNoSealKey, SealKeyLength)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return Sealer{}, fmt.Errorf("identity: building the cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return Sealer{}, fmt.Errorf("identity: building the cipher: %w", err)
	}
	return Sealer{block: aead}, nil
}

// Configured reports whether this deployment can hold a client secret at all.
func (s Sealer) Configured() bool { return s.block != nil }

// Seal encrypts a client secret for storage. The nonce is random per call and prepended, so
// sealing the same secret twice produces different bytes and the column leaks nothing by
// comparison.
func (s Sealer) Seal(plaintext string) ([]byte, error) {
	if !s.Configured() {
		return nil, ErrNoSealKey
	}
	nonce := make([]byte, s.block.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("identity: minting a nonce: %w", err)
	}
	return s.block.Seal(nonce, nonce, []byte(plaintext), nil), nil
}

// Open reads a stored client secret back. A value that does not authenticate is refused rather
// than returned corrupted: GCM's tag is what tells a rotated or wrong key apart from a
// tampered column, and both are failures rather than a secret to present to an identity
// provider.
func (s Sealer) Open(sealed []byte) (string, error) {
	if !s.Configured() {
		return "", ErrNoSealKey
	}
	size := s.block.NonceSize()
	if len(sealed) < size {
		return "", errors.New("identity: the sealed value is too short to hold a nonce")
	}
	plaintext, err := s.block.Open(nil, sealed[:size], sealed[size:], nil)
	if err != nil {
		// The cause is deliberately dropped. It distinguishes a wrong key from a corrupted
		// value, and neither is a fact worth putting where a log aggregator can read it.
		return "", errors.New("identity: the stored client secret could not be opened")
	}
	return string(plaintext), nil
}
