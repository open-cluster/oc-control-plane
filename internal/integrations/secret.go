package integrations

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"unicode"
)

const (
	MinSecretLength     = 32
	MaxSecretLength     = 256
	MaxCredentialLength = 512
	generatedBytes      = 32
)

var ErrWeakSecret = errors.New("secret is too weak to configure")

func GenerateSecret() (string, error) {
	raw := make([]byte, generatedBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generating a webhook secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// Digest is what the database holds. SHA-256 rather than a slow key derivation.
func Digest(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// CheckCredentialShape refuses a pasted credential that cannot be one: empty, oversized,
// or carrying characters that would not survive an HTTP header.
func CheckCredentialShape(credential string) error {
	if credential == "" {
		return fmt.Errorf("%w: it must not be empty", ErrWeakSecret)
	}
	if len(credential) > MaxCredentialLength {
		return fmt.Errorf("%w: it must be at most %d characters", ErrWeakSecret, MaxCredentialLength)
	}
	for _, character := range credential {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return fmt.Errorf(
				"%w: it must not contain whitespace or control characters", ErrWeakSecret)
		}
	}
	return nil
}

func CheckSecretStrength(secret string) error {
	if len(secret) < MinSecretLength {
		return fmt.Errorf("%w: it must be at least %d characters", ErrWeakSecret, MinSecretLength)
	}
	if len(secret) > MaxSecretLength {
		return fmt.Errorf("%w: it must be at most %d characters", ErrWeakSecret, MaxSecretLength)
	}
	for _, character := range secret {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return fmt.Errorf(
				"%w: it must not contain whitespace or control characters", ErrWeakSecret)
		}
	}
	return nil
}
