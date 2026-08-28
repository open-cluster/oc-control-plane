package authz

import (
	"net/http"
	"net/url"
	"strings"
)

// originIsAllowed applies CSRF protection to cookie-authenticated unsafe requests.
func (g Guard) originIsAllowed(principal Principal, request *http.Request) bool {
	if principal.Kind() != KindUser {
		return true
	}
	switch request.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}

	origin := strings.TrimSpace(request.Header.Get("Origin"))
	if origin == "" {
		return false
	}
	for _, allowed := range g.Origins {
		if sameOrigin(origin, allowed) {
			return true
		}
	}
	return false
}

func sameOrigin(presented, allowed string) bool {
	first, err := url.Parse(presented)
	if err != nil || !isOrigin(first) {
		return false
	}
	second, err := url.Parse(strings.TrimSpace(allowed))
	if err != nil || !isOrigin(second) {
		return false
	}
	return strings.EqualFold(first.Scheme, second.Scheme) &&
		strings.EqualFold(first.Hostname(), second.Hostname()) &&
		portOf(first) == portOf(second)
}

func isOrigin(parsed *url.URL) bool {
	return parsed.User == nil && parsed.Opaque == "" && parsed.Host != "" &&
		(strings.EqualFold(parsed.Scheme, "http") || strings.EqualFold(parsed.Scheme, "https")) &&
		(parsed.Path == "" || parsed.Path == "/") && parsed.RawQuery == "" && parsed.Fragment == ""
}

func portOf(parsed *url.URL) string {
	if port := parsed.Port(); port != "" {
		return port
	}
	if strings.EqualFold(parsed.Scheme, "https") {
		return "443"
	}
	return "80"
}
