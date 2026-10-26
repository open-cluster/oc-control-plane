package config

import (
	"fmt"
	"net"
	"net/netip"
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
	if err != nil || !parsed.IsAbs() || parsed.Host == "" ||
		parsed.Opaque != "" || parsed.User != nil || strings.ContainsAny(trimmed, "?#") ||
		(parsed.EscapedPath() != "" && parsed.EscapedPath() != "/") {
		return "", fmt.Errorf("%s must be an absolute origin such as https://console.example.com",
			key)
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	host := strings.ToLower(parsed.Hostname())
	address, addressErr := netip.ParseAddr(host)
	if addressErr == nil && address.Is6() {
		host = address.String()
		if address.Is4In6() {
			// Browser origins retain IPv6 identity and serialize the mapped address in hexadecimal.
			octets := address.As16()
			host = fmt.Sprintf("::ffff:%x:%x", uint16(octets[12])<<8|uint16(octets[13]),
				uint16(octets[14])<<8|uint16(octets[15]))
		}
	}
	loopback := host == "localhost" || host == "127.0.0.1" || host == "::1"
	if parsed.Scheme != "https" && (parsed.Scheme != "http" || !loopback) {
		return "", fmt.Errorf("%s must be https; %s", key, insecureReason)
	}
	bracketed := strings.HasPrefix(parsed.Host, "[")
	if !validOriginHost(host) || bracketed != (addressErr == nil && address.Is6()) ||
		strings.HasSuffix(parsed.Host, ":") {
		return "", fmt.Errorf("%s must have a valid host and optional port", key)
	}
	port := parsed.Port()
	switch {
	case port != "":
		number, portErr := strconv.Atoi(port)
		if portErr != nil || number < 1 || number > 65535 {
			return "", fmt.Errorf("%s must have a port between 1 and 65535", key)
		}
		parsed.Host = net.JoinHostPort(host, strconv.Itoa(number))
	case bracketed:
		parsed.Host = "[" + host + "]"
	default:
		parsed.Host = host
	}
	parsed.Path, parsed.RawPath = "", ""
	return parsed.String(), nil
}

func validOriginHost(host string) bool {
	if net.ParseIP(host) != nil {
		return true
	}
	host = strings.TrimSuffix(host, ".")
	if host == "" || len(host) > 253 {
		return false
	}
	labels := strings.Split(host, ".")
	last := labels[len(labels)-1]
	// Browsers interpret numeric final labels as IPv4 rather than DNS names.
	if strings.Trim(last, "0123456789") == "" || strings.HasPrefix(last, "0x") &&
		strings.Trim(last[2:], "0123456789abcdef") == "" {
		return false
	}
	for _, label := range labels {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			valid := character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-'
			if !valid {
				return false
			}
		}
	}
	return true
}
