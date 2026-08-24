// Package seal holds the one mechanism for a credential that must be presented rather
// than compared: versioned AES-256-GCM envelopes under deployment keys.
package seal

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

const (
	KeyLength             = 32
	LegacyKeyVersion byte = 1
	EnvelopeVersion  byte = 2
	KeyVersion            = EnvelopeVersion
	maxKeyIDLength        = 64
)

var ErrNoKey = errors.New("no sealing key is configured")

// Key names one AES-256 key. ID is durable envelope metadata, not secret material.
type Key struct {
	ID       string
	Material []byte
}

// Sealer writes with one active key and reads every retained key.
type Sealer struct {
	active string
	keys   map[string]cipher.AEAD
}

// New preserves the original one-key construction boundary and can read version-1 rows.
func New(material []byte) (Sealer, error) {
	return NewKeyring(Key{ID: "default", Material: material})
}

// NewKeyring builds a sealer whose first key is active and remaining keys are read-only.
func NewKeyring(active Key, previous ...Key) (Sealer, error) {
	all := append([]Key{active}, previous...)
	keys := make(map[string]cipher.AEAD, len(all))
	for _, key := range all {
		if !validKeyID(key.ID) {
			return Sealer{}, errors.New("seal: key id must contain 1-64 letters, digits, '.', '_' or '-'")
		}
		if _, exists := keys[key.ID]; exists {
			return Sealer{}, fmt.Errorf("seal: key id %q is configured more than once", key.ID)
		}
		if len(key.Material) != KeyLength {
			return Sealer{}, fmt.Errorf("%w: key %q must be exactly %d bytes", ErrNoKey, key.ID, KeyLength)
		}
		block, err := aes.NewCipher(append([]byte(nil), key.Material...))
		if err != nil {
			return Sealer{}, fmt.Errorf("seal: building key %q: %w", key.ID, err)
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return Sealer{}, fmt.Errorf("seal: building key %q: %w", key.ID, err)
		}
		keys[key.ID] = aead
	}
	return Sealer{active: active.ID, keys: keys}, nil
}

func validKeyID(id string) bool {
	if len(id) == 0 || len(id) > maxKeyIDLength {
		return false
	}
	for _, character := range id {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '.' || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}

func (s Sealer) Configured() bool { return s.active != "" && len(s.keys) > 0 }

// ActiveKeyID is the durable identifier new envelopes name.
func (s Sealer) ActiveKeyID() string { return s.active }

// Seal encrypts under the active key and binds the ciphertext to its owning record.
func (s Sealer) Seal(plaintext string, binding []byte) ([]byte, error) {
	if !s.Configured() {
		return nil, ErrNoKey
	}
	block := s.keys[s.active]
	nonce := make([]byte, block.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("seal: minting a nonce: %w", err)
	}
	header := make([]byte, 0, 2+len(s.active)+len(nonce))
	header = append(header, EnvelopeVersion, byte(len(s.active)))
	header = append(header, s.active...)
	header = append(header, nonce...)
	return block.Seal(header, nonce, []byte(plaintext), binding), nil
}

// Open authenticates either the current envelope or the version-1 compatibility envelope.
func (s Sealer) Open(sealed []byte, binding []byte) (string, error) {
	if !s.Configured() {
		return "", ErrNoKey
	}
	if len(sealed) == 0 {
		return "", errors.New("seal: the sealed value is empty")
	}
	switch sealed[0] {
	case LegacyKeyVersion:
		return s.openLegacy(sealed, binding)
	case EnvelopeVersion:
		return s.openCurrent(sealed, binding)
	default:
		return "", fmt.Errorf("seal: unsupported envelope version %d", sealed[0])
	}
}

func (s Sealer) openCurrent(sealed []byte, binding []byte) (string, error) {
	keyID, offset, err := envelopeKeyID(sealed)
	if err != nil {
		return "", err
	}
	block, exists := s.keys[keyID]
	if !exists {
		return "", fmt.Errorf("seal: key %q is not configured", keyID)
	}
	if len(sealed) < offset+block.NonceSize()+block.Overhead() {
		return "", errors.New("seal: the sealed value is too short")
	}
	nonce := sealed[offset : offset+block.NonceSize()]
	plaintext, openErr := block.Open(nil, nonce, sealed[offset+block.NonceSize():], binding)
	if openErr != nil {
		return "", errors.New("seal: the stored secret could not be opened")
	}
	return string(plaintext), nil
}

func (s Sealer) openLegacy(sealed []byte, binding []byte) (string, error) {
	for _, block := range s.keys {
		if len(sealed) < 1+block.NonceSize()+block.Overhead() {
			continue
		}
		nonce := sealed[1 : 1+block.NonceSize()]
		plaintext, err := block.Open(nil, nonce, sealed[1+block.NonceSize():], binding)
		if err == nil {
			return string(plaintext), nil
		}
	}
	return "", errors.New("seal: the stored secret could not be opened")
}

// EnvelopeKeyID reports the non-secret key identifier on a current envelope.
func EnvelopeKeyID(sealed []byte) (string, error) {
	keyID, _, err := envelopeKeyID(sealed)
	return keyID, err
}

func envelopeKeyID(sealed []byte) (string, int, error) {
	if len(sealed) < 2 || sealed[0] != EnvelopeVersion {
		return "", 0, errors.New("seal: the value is not a current envelope")
	}
	length := int(sealed[1])
	if length == 0 || length > maxKeyIDLength || len(sealed) < 2+length {
		return "", 0, errors.New("seal: the envelope has an invalid key id")
	}
	keyID := string(sealed[2 : 2+length])
	if !validKeyID(keyID) {
		return "", 0, errors.New("seal: the envelope has an invalid key id")
	}
	return keyID, 2 + length, nil
}

// Rewrap returns an envelope under the active key.
func (s Sealer) Rewrap(sealed []byte, binding []byte) ([]byte, bool, error) {
	plaintext, err := s.Open(sealed, binding)
	if err != nil {
		return nil, false, err
	}
	if keyID, keyErr := EnvelopeKeyID(sealed); keyErr == nil && keyID == s.active {
		return append([]byte(nil), sealed...), false, nil
	}
	rewrapped, err := s.Seal(plaintext, binding)
	if err != nil {
		return nil, false, err
	}
	return rewrapped, true, nil
}
