package integrations

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"unicode"
)

// Webhook secret bounds. The platform generates every secret it issues, so the minimum is
// not a defence against a human choosing a bad one — it is a floor an operator supplying
// their own must clear, because an Integration whose secret is guessable is one anyone on
// the internet can deliver alerts through.
const (
	MinSecretLength = 32
	// MaxSecretLength bounds what may be presented, so a header cannot be used to make
	// every delivery hash a megabyte.
	MaxSecretLength = 256
	generatedBytes  = 32
)

// ErrWeakSecret reports a secret that would not be worth having. It names what is wrong,
// because the caller is an operator configuring their own tenant rather than someone
// probing one.
var ErrWeakSecret = errors.New("secret is too weak to configure")

// GenerateSecret mints the shared secret a webhook source will present. It is returned to
// the operator exactly once and stored only as a digest, so no path reads it back and a
// disclosure of the database yields no ability to forge a delivery.
func GenerateSecret() (string, error) {
	raw := make([]byte, generatedBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generating a webhook secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// Digest is what the database holds. SHA-256 rather than a slow key derivation: the cost of
// making verification expensive is paid on every delivery, and it buys resistance to
// offline attack on a LOW-ENTROPY human secret. These are high-entropy and
// platform-generated, so there is nothing for that cost to protect.
func Digest(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// MintFingerprint mints an identity for a secret. Minted, never derived: a truncated hash
// would let anyone holding a database dump confirm a guess offline.
func MintFingerprint() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("minting a secret fingerprint: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// CheckSecretStrength refuses a secret that could not survive being guessed at. Only
// length and character range are checked: anything cleverer is theatre on a value no human
// types.
func CheckSecretStrength(secret string) error {
	if len(secret) < MinSecretLength {
		return fmt.Errorf("%w: it must be at least %d characters", ErrWeakSecret, MinSecretLength)
	}
	if len(secret) > MaxSecretLength {
		return fmt.Errorf("%w: it must be at most %d characters", ErrWeakSecret, MaxSecretLength)
	}
	for _, character := range secret {
		// A control character or whitespace cannot survive a round trip through an HTTP
		// header intact, so a secret containing one would authenticate inconsistently.
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return fmt.Errorf(
				"%w: it must not contain whitespace or control characters", ErrWeakSecret)
		}
	}
	return nil
}
