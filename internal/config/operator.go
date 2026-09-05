package config

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
)

const minOperatorTokenLength = 32

func operatorTokenDigest(
	lookup func(string) (string, bool), operatorAddress string,
) ([]byte, error) {
	path, _ := lookup(EnvOperatorTokenFile)
	path = strings.TrimSpace(path)

	if operatorAddress == "" {
		return nil, nil
	}
	if path == "" {
		return nil, fmt.Errorf("%s is required when %s is set",
			EnvOperatorTokenFile, EnvHTTPAddress)
	}

	token, err := readSecretFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", EnvOperatorTokenFile, err)
	}
	if len(token) < minOperatorTokenLength {
		return nil, fmt.Errorf("%s: the token must be at least %d characters; the surface it "+
			"guards reads across every tenant this instance serves",
			EnvOperatorTokenFile, minOperatorTokenLength)
	}
	digest := sha256.Sum256([]byte(token))
	return digest[:], nil
}

const defaultOperatorTokenRole = "admin"

// optionalBrowserURL reads a URL a browser will be sent to or arrive from. It must be an
// absolute origin with no path, because everything downstream appends one.
func optionalBrowserURL(lookup func(string) (string, bool), key string) (string, error) {
	return optionalOrigin(lookup, key, "a session cookie is Secure and would never reach a "+
		"plaintext origin")
}

// optionalOrigin reads an absolute origin, refusing a plaintext one for the stated reason.
//
// One parser for both, because the two differ only in WHY http is refused — and a second copy of
// the parsing is a second place a path component or a missing scheme could slip through.
func optionalOrigin(
	lookup func(string) (string, bool), key, insecureReason string,
) (string, error) {
	value, ok := lookup(key)
	if !ok || strings.TrimSpace(value) == "" {
		return "", nil
	}
	trimmed := strings.TrimSuffix(strings.TrimSpace(value), "/")
	parsed, err := url.Parse(trimmed)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" ||
		(parsed.Path != "" && parsed.Path != "/") {
		return "", fmt.Errorf("%s must be an absolute origin such as https://console.example.com",
			key)
	}
	if parsed.Scheme != "https" && parsed.Hostname() != "localhost" &&
		parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "::1" {
		return "", fmt.Errorf("%s must be https; %s", key, insecureReason)
	}
	return trimmed, nil
}

func sealingKey(lookup func(string) (string, bool)) ([]byte, error) {
	path, _ := lookup(EnvSealingKeyFile)
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	active, err := readSealingKey(EnvSealingKeyFile, strings.TrimSpace(path))
	if err != nil {
		return nil, err
	}
	return active, nil
}

func readSealingKey(setting, path string) ([]byte, error) {
	raw, err := (MountedSecretSource{}).Read(setting, path)
	if err != nil {
		return nil, err
	}

	trimmed := strings.TrimSpace(string(raw))
	if decoded, decodeErr := base64.StdEncoding.DecodeString(trimmed); decodeErr == nil &&
		len(decoded) == sealingKeyLength {
		return decoded, nil
	}
	if len(raw) == sealingKeyLength {
		return raw, nil
	}
	return nil, fmt.Errorf("%s: the key must be %d bytes, raw or base64-encoded",
		setting, sealingKeyLength)
}

// sealingKeyLength is AES-256's key size. It is stated here rather than imported so that
// configuration parses without depending on the package that uses the key.
const sealingKeyLength = 32
