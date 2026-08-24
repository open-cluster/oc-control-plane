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

// KeyVersion is the version every seal writes as the blob's leading byte. There is one
// held key today; when rotation arrives, a new version's key joins the sealer and rows
// are re-wrapped under it one by one — the byte is what makes that a migration instead
// of a mass of blobs nothing can attribute to a key.
const KeyVersion byte = 1

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
// the same shape a database's DSN is, and never from an environment value.
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

// Seal encrypts a secret for storage as [key version][nonce][ciphertext]. The nonce is
// random per call, so sealing the same secret twice produces different bytes and the
// column leaks nothing by comparison.
//
// binding ties the sealed bytes to the row they belong to — an integration's ID — as
// GCM additional authenticated data: a blob copied onto another row refuses to open
// there. Open must be given the same binding. nil is a secret whose row identity does
// not exist at seal time; it is bound to nothing.
func (s Sealer) Seal(plaintext string, binding []byte) ([]byte, error) {
	if !s.Configured() {
		return nil, ErrNoKey
	}
	nonce := make([]byte, s.block.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("seal: minting a nonce: %w", err)
	}
	header := append([]byte{KeyVersion}, nonce...)
	return s.block.Seal(header, nonce, []byte(plaintext), binding), nil
}

// Open reads a stored secret back, under the same binding it was sealed with. A value
// that does not authenticate is refused rather than returned corrupted: GCM's tag is
// what tells a wrong key, a wrong binding and a tampered column apart from a secret,
// and all three are failures rather than something to present anywhere.
func (s Sealer) Open(sealed []byte, binding []byte) (string, error) {
	if !s.Configured() {
		return "", ErrNoKey
	}
	if len(sealed) == 0 {
		return "", errors.New("seal: the sealed value is empty")
	}
	if sealed[0] != KeyVersion {
		// Named so a half-rotated deployment self-diagnoses: the row says which key it
		// needs, and this deployment does not hold it.
		return "", fmt.Errorf("seal: the value is sealed under key version %d, which this "+
			"deployment does not hold", sealed[0])
	}
	size := s.block.NonceSize()
	if len(sealed) < 1+size {
		return "", errors.New("seal: the sealed value is too short to hold a nonce")
	}
	plaintext, err := s.block.Open(nil, sealed[1:1+size], sealed[1+size:], binding)
	if err != nil {
		// The cause is deliberately dropped. It distinguishes a wrong key from a corrupted
		// value, and neither is a fact worth putting where a log aggregator can read it.
		return "", errors.New("seal: the stored secret could not be opened")
	}
	return string(plaintext), nil
}
