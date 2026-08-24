package config

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// The operator surface's own settings: the bootstrap credential and its scope, where the
// surface and its console are reachable from a browser, and the key presentable
// credentials are sealed under.
//
// They are here rather than in config.go because they are one subject and because config.go
// was past the length this repository wants a file to be. Nothing about the parsing differs;
// what changed is that the settings deciding WHO MAY REACH THE PRODUCT are read in one place a
// reviewer can hold in their head.

// minOperatorTokenLength is the shortest token the operator surface will accept.
//
// The surface it guards reads across every tenant this instance serves, so a token short
// enough to be guessed is the same as no token at all. Refusing a weak one at startup makes
// that a deployment that fails to start rather than one that runs and looks fine.
const minOperatorTokenLength = 32

// operatorTokenDigest reads the operator credential from its file and returns only the digest.
//
// The variable names a path, never the token, for the same reason placements name a DSN file:
// an environment value is visible to anything that can read the process's environment, ends up
// in orchestrator manifests, and is printed by half the tooling that touches a container. No
// error here quotes the file's contents.
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
			EnvOperatorTokenFile, EnvOperatorAddress)
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

// operatorCredentialScope binds the bootstrap credential to one tenant and one role.
//
// The organization is REQUIRED whenever a token is configured. There is deliberately no
// default: a credential whose scope was inferred is the ambient root credential this slice
// exists to replace, and the deployment that would get it is the one that did not think about
// it. The role defaults to the owner, because a deployment with no members yet needs a
// credential that can create the first one, and narrowing it is a one-line change the operator
// can make once they have.
func operatorCredentialScope(lookup func(string) (string, bool), cfg *Config) error {
	if len(cfg.OperatorTokenDigest) == 0 {
		return nil
	}

	organization, _ := lookup(EnvOperatorTokenOrganization)
	cfg.OperatorTokenOrganization = strings.TrimSpace(organization)
	if cfg.OperatorTokenOrganization == "" {
		return fmt.Errorf("%s is required when %s is set: the credential is bound to one "+
			"organization, because a token that reaches every tenant is the thing it replaces",
			EnvOperatorTokenOrganization, EnvOperatorTokenFile)
	}

	role, _ := lookup(EnvOperatorTokenRole)
	cfg.OperatorTokenRole = strings.TrimSpace(role)
	if cfg.OperatorTokenRole == "" {
		cfg.OperatorTokenRole = defaultOperatorTokenRole
	}
	return nil
}

// defaultOperatorTokenRole is what the bootstrap credential holds when a deployment says
// nothing. It is named here rather than imported from internal/authz because configuration
// must not depend on the authorization model to parse; the composition root refuses an
// unrecognised value when it builds the principal.
const defaultOperatorTokenRole = "admin"

// optionalBrowserURL reads a URL a browser will be sent to or arrive from. It must be an
// absolute origin with no path, because everything downstream appends one.
func optionalBrowserURL(lookup func(string) (string, bool), key string) (string, error) {
	return optionalOrigin(lookup, key, "a session cookie is Secure and would never reach a "+
		"plaintext origin")
}

// optionalIntakeURL reads the public origin a customer's own alerting reaches intake at.
//
// It is CONFIGURED rather than derived from a request, because the delivery endpoint this
// produces is pasted into somebody else's system: a URL assembled from the operator surface's
// own Host header would be one that works from wherever the console is served and not from the
// customer's alerting, which is the one place it has to work.
//
// Empty is supported and means the delivery endpoint is served as an absence rather than as a
// guess. That is the honest answer for a deployment that has not been told where it is reachable.
func optionalIntakeURL(lookup func(string) (string, bool), key string) (string, error) {
	return optionalOrigin(lookup, key, "a source presents its verification secret to this "+
		"URL, and a plaintext origin would put that secret on the wire")
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

// allowedOrigins reads where a browser may make an unsafe request from.
func allowedOrigins(lookup func(string) (string, bool)) ([]string, error) {
	raw, _ := lookup(EnvOperatorAllowedOrigins)
	origins := make([]string, 0, 2)
	entries, listErr := decodeList(raw)
	if listErr != nil {
		return nil, fmt.Errorf("%s: invalid list: %w", EnvOperatorAllowedOrigins, listErr)
	}
	for _, entry := range entries {
		trimmed := strings.TrimSuffix(strings.TrimSpace(entry), "/")
		if trimmed == "" {
			continue
		}
		parsed, err := url.Parse(trimmed)
		if err != nil || !parsed.IsAbs() || parsed.Host == "" {
			return nil, fmt.Errorf("%s: each origin must be a scheme and a host, such as "+
				"https://console.example.com", EnvOperatorAllowedOrigins)
		}
		origins = append(origins, trimmed)
	}
	return origins, nil
}

// sealingKey reads the key presentable credentials are sealed under.
//
// The file holds the key as base64 or as raw bytes; both are accepted because a key generated
// with openssl and one generated with head -c 32 are both what an operator will reach for. No
// error here quotes the file's contents.
func sealingKey(lookup func(string) (string, bool)) ([]byte, error) {
	path, _ := lookup(EnvSealingKeyFile)
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(strings.TrimSpace(path))
	if err != nil {
		return nil, fmt.Errorf("%s: the key file could not be read", EnvSealingKeyFile)
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
		EnvSealingKeyFile, sealingKeyLength)
}

// sealingKeyLength is AES-256's key size. It is stated here rather than imported so that
// configuration parses without depending on the package that uses the key.
const sealingKeyLength = 32
