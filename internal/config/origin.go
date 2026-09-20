package config

import (
	"fmt"
	"net/url"
	"strings"
)

// optionalBrowserURL reads a URL a browser will be sent to or arrive from. It must be an
// absolute origin with no path, because everything downstream appends one.
func optionalBrowserURL(lookup func(string) (string, bool), key string) (string, error) {
	return optionalOrigin(lookup, key, "a session cookie is Secure and would never reach a "+
		"plaintext origin")
}

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
