package config

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

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
	trimmed := strings.TrimSpace(value)
	parsed, err := url.Parse(trimmed)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || parsed.User != nil ||
		parsed.Opaque != "" || (parsed.EscapedPath() != "" && parsed.EscapedPath() != "/") ||
		parsed.ForceQuery || parsed.RawQuery != "" || strings.Contains(trimmed, "#") {
		return "", fmt.Errorf("%s must be an absolute origin such as https://console.example.com",
			key)
	}
	host := strings.ToLower(parsed.Hostname())
	if !validOriginHost(host) || strings.HasSuffix(parsed.Host, ":") ||
		strings.HasPrefix(parsed.Host, "[") && net.ParseIP(host) == nil {
		return "", fmt.Errorf("%s must have a valid host and port", key)
	}
	port := parsed.Port()
	if port != "" {
		number, portErr := strconv.Atoi(port)
		if portErr != nil || number < 1 || number > 65535 {
			return "", fmt.Errorf("%s must have a valid host and port", key)
		}
		port = strconv.Itoa(number)
	}
	scheme := strings.ToLower(parsed.Scheme)
	loopback := host == "localhost" || host == "127.0.0.1" || host == "::1"
	if scheme != "https" && (scheme != "http" || !loopback) {
		return "", fmt.Errorf("%s must be https or an approved loopback http origin; %s", key, insecureReason)
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return scheme + "://" + host, nil
}

func validOriginHost(host string) bool {
	if net.ParseIP(host) != nil {
		return true
	}
	if host == "" || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(strings.TrimSuffix(host, "."), ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '-' {
				continue
			}
			return false
		}
	}
	return true
}
