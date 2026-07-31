package connection

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"unicode"
)

// Secret bounds. The platform generates every secret it issues, so the minimum is not a
// defence against a human choosing a bad one — it is a floor an operator supplying their own
// must clear, and it exists because a Connection whose secret is guessable is a Connection
// anyone on the internet can deliver alerts through.
const (
	// MinSecretLength is thirty-two characters. A generated secret is well above it; the floor
	// is what makes a supplied one refusable at creation rather than a weakness discovered
	// during an incident.
	MinSecretLength = 32
	// MaxSecretLength bounds what may be presented, so a header cannot be used to make every
	// delivery hash a megabyte.
	MaxSecretLength = 256
	// generatedBytes is the entropy behind an issued secret. Thirty-two bytes is beyond
	// brute force and renders to a header value of a workable length.
	generatedBytes = 32
)

// ErrWeakSecret reports a secret that would not be worth having. It names what is wrong,
// because the caller is an operator configuring their own tenant rather than someone probing
// one, and telling them "too short" costs nothing and saves a support conversation.
var ErrWeakSecret = errors.New("secret is too weak to configure")

// GenerateSecret mints the shared secret a trigger Connection's source will present.
//
// It is returned to the operator exactly once and stored only as a digest, so no path reads it
// back and a disclosure of the database yields no ability to forge a delivery. A failure to
// read entropy is returned rather than absorbed: a credential minted from a degraded source is
// worse than no credential, because it looks exactly like a good one.
func GenerateSecret() (string, error) {
	raw := make([]byte, generatedBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generating a connection secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// Digest is what the database holds. SHA-256 rather than a slow key derivation, and for a
// stated reason: the cost of making verification expensive is paid on every delivery, and it
// buys resistance to offline attack on a LOW-ENTROPY human secret. These are high-entropy and
// platform-generated, so there is nothing for that cost to protect.
func Digest(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// CheckSecretStrength refuses a secret that could not survive being guessed at.
//
// Only length and character range are checked. Anything cleverer — dictionaries, entropy
// estimates, "must contain a symbol" — is theatre on a value no human types, and the rules
// would mostly serve to refuse perfectly good generated secrets.
func CheckSecretStrength(secret string) error {
	if len(secret) < MinSecretLength {
		return fmt.Errorf("%w: it must be at least %d characters", ErrWeakSecret, MinSecretLength)
	}
	if len(secret) > MaxSecretLength {
		return fmt.Errorf("%w: it must be at most %d characters", ErrWeakSecret, MaxSecretLength)
	}
	for _, character := range secret {
		// A control character or whitespace cannot survive a round trip through an HTTP header
		// intact, so a secret containing one would authenticate inconsistently — which reads as
		// an intermittent outage rather than as the configuration mistake it is.
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return fmt.Errorf(
				"%w: it must not contain whitespace or control characters", ErrWeakSecret)
		}
	}
	return nil
}
