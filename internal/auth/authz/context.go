package authz

import (
	"context"
)

// principalKey is the context key the guard leaves the resolved principal under. It is an
// unexported type so nothing outside this package can collide with it or set one.
type principalKey struct{}

// WithPrincipal returns a context carrying the principal resolved at an authentication boundary.
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
