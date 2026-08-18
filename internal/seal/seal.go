// Package seal holds the one mechanism for a credential that must be PRESENTED rather
// than compared: AES-256-GCM under a key the deployment names as a file. Every other
// credential in this product is digested, because it is only ever compared against; a
// secret that has to be read back — an identity provider's client secret, an
// Integration's outbound credential — is sealed with this instead.
package seal

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

// KeyLength is the key size this build uses. AES-256 rather than AES-128 because the key
// is configuration read from a file, so the larger one costs nothing anybody notices.
const KeyLength = 32

// ErrNoKey reports a deployment asked to hold a presentable secret with no key configured
// to seal it under. It is a refusal rather than a fallback to storing the secret in the
// clear.
var ErrNoKey = errors.New("no sealing key is configured")

// Sealer holds the key presentable secrets are stored under.
//
// A sealed secret is ENCRYPTED rather than digested because it has to be PRESENTED —
// to an identity provider's token endpoint, to a vendor's API — so the process must be
// able to read it back; every other credential here is only ever compared against, and
// a one-way digest suffices.
//
// That makes the key the thing that matters. It is read from a file the deployment names, in
// the same shape a placement's DSN is, and never from an environment value.
type Sealer struct {
	block cipher.AEAD
}

// New builds a sealer from the configured key.
func New(key []byte) (Sealer, error) {
	if len(key) != KeyLength {
		return Sealer{}, fmt.Errorf("%w: the key must be exactly %d bytes",
			ErrNoKey, KeyLength)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return Sealer{}, fmt.Errorf("seal: building the cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return Sealer{}, fmt.Errorf("seal: building the cipher: %w", err)
	}
	return Sealer{block: aead}, nil
}

// Configured reports whether this deployment can hold a presentable secret at all.
func (s Sealer) Configured() bool { return s.block != nil }

// Seal encrypts a secret for storage. The nonce is random per call and prepended, so
// sealing the same secret twice produces different bytes and the column leaks nothing by
// comparison.
func (s Sealer) Seal(plaintext string) ([]byte, error) {
	if !s.Configured() {
		return nil, ErrNoKey
	}
	nonce := make([]byte, s.block.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("seal: minting a nonce: %w", err)
	}
	return s.block.Seal(nonce, nonce, []byte(plaintext), nil), nil
}

// Open reads a stored secret back. A value that does not authenticate is refused rather
// than returned corrupted: GCM's tag is what tells a rotated or wrong key apart from a
// tampered column, and both are failures rather than a secret to present anywhere.
func (s Sealer) Open(sealed []byte) (string, error) {
	if !s.Configured() {
		return "", ErrNoKey
	}
	size := s.block.NonceSize()
	if len(sealed) < size {
		return "", errors.New("seal: the sealed value is too short to hold a nonce")
	}
	plaintext, err := s.block.Open(nil, sealed[:size], sealed[size:], nil)
	if err != nil {
		// The cause is deliberately dropped. It distinguishes a wrong key from a corrupted
		// value, and neither is a fact worth putting where a log aggregator can read it.
		return "", errors.New("seal: the stored secret could not be opened")
	}
	return string(plaintext), nil
}
