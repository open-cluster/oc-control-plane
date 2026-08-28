package authz

import (
	"context"
	"net/http"

	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
)

// principalKey is the context key the guard leaves the resolved principal under. It is an
// unexported type so nothing outside this package can collide with it or set one.
type principalKey struct{}

type activeOrganizationKey struct{}

// WithPrincipal returns a context carrying the principal. Only the guard calls it: a handler
// that could put a principal into its own context could put one there that no credential
// resolved to.
func WithPrincipal(ctx context.Context, principal Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, principal)
}

// PrincipalFrom reports who the guard resolved for this request.
//
// A handler behind the guard always has one, so the second return is a programming error
// rather than a runtime condition — a handler mounted outside the route table. It is returned
// rather than panicked on so that the failure is a 500 with a log line instead of a process
// that dies on a route somebody forgot to register.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	principal, ok := ctx.Value(principalKey{}).(Principal)
	return principal, ok && !principal.IsZero()
}

// Of is PrincipalFrom for a request, which is what every handler actually has to hand.
func Of(request *http.Request) (Principal, bool) { return PrincipalFrom(request.Context()) }

func withActiveOrganization(
	ctx context.Context, organization tenancy.Organization,
) context.Context {
	return context.WithValue(ctx, activeOrganizationKey{}, organization)
}

// ActiveOrganizationFrom reports the Organization verified for an Organization-scoped route.
func ActiveOrganizationFrom(ctx context.Context) (tenancy.Organization, bool) {
	organization, ok := ctx.Value(activeOrganizationKey{}).(tenancy.Organization)
	return organization, ok && !organization.IsEmpty()
}

// requestIDKey is where the operator surface's correlation identifier lives.
type requestIDKey struct{}

// WithRequestID returns a context carrying the identifier that ties this request's audit
// events to its log lines. The surface's own middleware sets it, once, before anything else
// runs; a handler that could set one could break the correlation it exists to provide.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestIDFrom reports this request's correlation identifier, or "" outside the surface.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}
