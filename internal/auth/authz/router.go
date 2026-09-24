package authz

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/audit"
)

// Route declares one protected application endpoint.
type Route struct {
	Method     string
	Pattern    string
	Permission Permission
	Handler    http.Handler
}

func routeID(route Route) string { return route.Method + " " + route.Pattern }

// Guard holds the dependencies needed to protect the application API.
type Guard struct {
	Resolve func(*http.Request) (Principal, error)
	Record  func(context.Context, uuid.UUID, audit.Event)
	Origin  string
	Logger  *slog.Logger
}

// Router validates and registers protected routes in one pass.
func Router(routes []Route, guard Guard) (http.Handler, error) {
	if len(routes) == 0 {
		return nil, fmt.Errorf("authz: the route table is empty")
	}
	if guard.Resolve == nil {
		return nil, fmt.Errorf("authz: the protected router has no identity resolver")
	}
	mux := http.NewServeMux()
	seen := make(map[string]struct{}, len(routes))
	for _, route := range routes {
		id := routeID(route)
		switch {
		case route.Handler == nil:
			return nil, fmt.Errorf("authz: route %q has no handler", id)
		case route.Method == "" || !strings.HasPrefix(route.Pattern, "/"):
			return nil, fmt.Errorf("authz: route %q is not a method and an absolute pattern", id)
		case route.Permission != "" && !Declared(route.Permission):
			return nil, fmt.Errorf("authz: route %q requires undeclared permission %q", id, route.Permission)
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, fmt.Errorf("authz: route %q is registered twice", id)
		}
		seen[id] = struct{}{}
		mux.Handle(id, guard.protect(route))
	}
	return mux, nil
}

func (g Guard) protect(route Route) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		principal, err := g.Resolve(request)
		if err != nil || principal.IsZero() {
			if errors.Is(err, ErrAuthenticationUnavailable) {
				writeJSON(writer, http.StatusServiceUnavailable, errorView{Error: "authentication unavailable"})
				return
			}
			g.refuseUnauthenticated(writer, request, err)
			return
		}
		if !g.originIsAllowed(request) {
			g.recordRefusal(request, principal, route, "origin not allowed")
			g.refuseOrigin(writer, request, principal)
			return
		}
		if route.Permission != "" && !principal.Can(route.Permission) {
			g.recordRefusal(request, principal, route, "role does not grant it")
			writeJSON(writer, http.StatusForbidden, errorView{
				Error: "forbidden", Requires: string(route.Permission),
			})
			return
		}
		ctx := WithPrincipal(request.Context(), principal)
		route.Handler.ServeHTTP(writer, request.WithContext(ctx))
	})
}

// An unexported context key prevents callers outside this package from installing a Principal.
type principalKey struct{}

// WithPrincipal returns a context carrying the Principal resolved at an authentication boundary.
func WithPrincipal(ctx context.Context, principal Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, principal)
}

// MustPrincipal returns the authenticated Principal installed by the protected router.
func MustPrincipal(ctx context.Context) Principal {
	principal, ok := ctx.Value(principalKey{}).(Principal)
	if !ok || principal.IsZero() {
		panic("authz: protected handler has no Principal")
	}
	return principal
}

func (g Guard) originIsAllowed(request *http.Request) bool {
	switch request.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return g.cookieOriginIsAllowed(request)
}

func (g Guard) cookieOriginIsAllowed(request *http.Request) bool {
	return CookieOriginAllowed(request, g.Origin)
}

// CookieOriginAllowed checks an unsafe cookie request against the configured browser origin.
func CookieOriginAllowed(request *http.Request, allowed string) bool {
	origin := strings.TrimSpace(request.Header.Get("Origin"))
	if origin == "" {
		return false
	}
	return sameOrigin(origin, allowed)
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
