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
	value, ok := lookup(key)
	if !ok || strings.TrimSpace(value) == "" {
		return "", nil
	}
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Host == "" || parsed.Opaque != "" || parsed.User != nil ||
		strings.ContainsAny(value, "?#") || (parsed.EscapedPath() != "" && parsed.EscapedPath() != "/") {
		return "", fmt.Errorf("%s must be an absolute origin without credentials, path, query or fragment", key)
	}
	host := strings.ToLower(parsed.Hostname())
	address, addressErr := netip.ParseAddr(host)
	if addressErr == nil {
		if address.Is4In6() || address.Zone() != "" {
			return "", fmt.Errorf("%s does not support mapped or scoped IP addresses", key)
		}
		host = address.String()
	} else {
		// Restrict DNS origins to ASCII names with an alphabetic final label;
		// ambiguous numeric hosts need a canonical IP address instead.
		last := strings.TrimSuffix(host, ".")
		last = last[strings.LastIndex(last, ".")+1:]
		if last == "" || last[0] < 'a' || last[0] > 'z' ||
			strings.Contains(host, ":") || url.PathEscape(host) != host {
			return "", fmt.Errorf("%s must use an ASCII hostname or a canonical IP address", key)
		}
	}
	bracketed := strings.HasPrefix(parsed.Host, "[")
	if bracketed != (addressErr == nil && address.Is6()) || strings.HasSuffix(parsed.Host, ":") {
		return "", fmt.Errorf("%s must have a valid host and optional port", key)
	}
	if parsed.Scheme != "https" && (parsed.Scheme != "http" ||
		(host != "localhost" && host != "127.0.0.1" && host != "::1")) {
		return "", fmt.Errorf("%s must use https, or http with localhost, 127.0.0.1 or [::1]", key)
	}
	if port := parsed.Port(); port != "" {
		if _, _, err := net.SplitHostPort(parsed.Host); err != nil {
			return "", fmt.Errorf("%s must have a valid host and port", key)
		}
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return "", fmt.Errorf("%s must have a port between 1 and 65535", key)
		}
		parsed.Host = net.JoinHostPort(host, strconv.Itoa(number))
	} else if bracketed {
		parsed.Host = "[" + host + "]"
	} else {
		parsed.Host = host
	}
	parsed.Path, parsed.RawPath = "", ""
	return parsed.String(), nil
}
