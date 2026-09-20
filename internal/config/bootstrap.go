package config

import (
	"crypto/sha256"
	"fmt"
)

const minBootstrapTokenLength = 32

func bootstrapTokenDigest(
	lookup func(string) (string, bool), serverAddress string,
) ([]byte, error) {
	if serverAddress == "" {
		return nil, nil
	}
	token, err := readSecretText(lookup, EnvBootstrapTokenFile)
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, nil
	}
	if len(token) < minBootstrapTokenLength {
		return nil, fmt.Errorf("%s or %s: the token must be at least %d characters",
			EnvBootstrapToken, EnvBootstrapTokenFile, minBootstrapTokenLength)
	}
	digest := sha256.Sum256([]byte(token))
	return digest[:], nil
}
