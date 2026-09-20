package config

import (
	"encoding/base64"
	"fmt"
	"strings"
)

const sealingKeyLength = 32

func sealingKey(lookup func(string) (string, bool)) ([]byte, error) {
	raw, err := readSecret(lookup, EnvSealingKeyFile)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	trimmed := strings.TrimSpace(string(raw))
	if decoded, decodeErr := base64.StdEncoding.DecodeString(trimmed); decodeErr == nil &&
		len(decoded) == sealingKeyLength {
		return decoded, nil
	}
	if len(raw) == sealingKeyLength {
		return raw, nil
	}
	return nil, fmt.Errorf("%s or %s: the key must be %d bytes, raw or base64-encoded",
		EnvSealingKey, EnvSealingKeyFile, sealingKeyLength)
}
