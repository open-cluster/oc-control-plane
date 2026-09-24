package authz

import (
	"context"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"log/slog"
	"net/http"
	"strings"

	"github.com/open-cluster/oc-control-plane/internal/audit"
)

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
