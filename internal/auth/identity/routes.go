package identity

import (
	"errors"
	"net/http"

	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/session"
)

// Base is where every route in this package hangs. Named once so a path correction is a
// one-line change rather than a search.
const Base = "/api/v1"

func (h Handlers) Routes() []authz.Route {
	return []authz.Route{
		{Method: http.MethodPut, Pattern: Base + "/auth/local/password", Handler: http.HandlerFunc(h.changeLocalPassword)},
		// Every authenticated member may inspect their own session.
		{Method: http.MethodGet, Pattern: Base + "/session", Handler: http.HandlerFunc(h.session)},
		{Method: http.MethodGet, Pattern: Base + "/permissions", Handler: http.HandlerFunc(h.permissions)},

		// Who they are once inside.
		{Method: http.MethodGet, Pattern: Base + "/members", Permission: authz.MemberRead, Handler: http.HandlerFunc(h.listMembers)},
		{Method: http.MethodPost, Pattern: Base + "/local-users", Permission: authz.MemberManage, Handler: http.HandlerFunc(h.createMember)},
		{Method: http.MethodPatch, Pattern: Base + "/members/{user}", Permission: authz.MemberManage, Handler: http.HandlerFunc(h.setMember)},
		{Method: http.MethodDelete, Pattern: Base + "/members/{user}", Permission: authz.MemberManage, Handler: http.HandlerFunc(h.removeMember)},

		// Live sessions and their revocation.
		{Method: http.MethodGet, Pattern: Base + "/sessions", Handler: http.HandlerFunc(h.listSessions)},
		{Method: http.MethodDelete, Pattern: Base + "/sessions/{session}", Handler: http.HandlerFunc(h.revokeSession)},

		// The tenant's own policy.
		{Method: http.MethodGet, Pattern: Base + "/policy", Permission: authz.IdentityRead, Handler: http.HandlerFunc(h.readPolicy)},
		{Method: http.MethodPut, Pattern: Base + "/policy", Permission: authz.IdentityConfigure, Handler: http.HandlerFunc(h.writePolicy)},

		// The record.
		{Method: http.MethodGet, Pattern: Base + "/audit-events", Permission: authz.AuditRead, Handler: http.HandlerFunc(h.auditEvents)},
	}
}

// Authentication is the public sign-in surface and authentication-owned sign-out handler.
type Authentication struct {
	LocalBootstrap http.Handler
	LocalSignIn    http.Handler
	OIDCStart      http.Handler
	OIDCCallback   http.Handler
	SignOut        http.Handler
}

// Authentication returns the public sign-in surface and authentication-owned sign-out handler.
func (h Handlers) Authentication(origin string) Authentication {
	return Authentication{
		LocalBootstrap: http.HandlerFunc(h.bootstrapLocalAdmin),
		LocalSignIn:    http.HandlerFunc(h.localSignIn),
		OIDCStart:      http.HandlerFunc(h.startDeploymentOIDCSignIn),
		OIDCCallback:   http.HandlerFunc(h.completeDeploymentOIDCSignIn),
		SignOut:        h.protectSignOut(origin),
	}
}

func (h Handlers) protectSignOut(origin string) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !authz.CookieOriginAllowed(request, origin) {
			writeJSON(writer, http.StatusForbidden, errorView{Error: "request origin is not allowed"})
			return
		}
		session.Clear(writer)
		principal, err := h.Resolve(request)
		if errors.Is(err, authz.ErrNoCredential) || errors.Is(err, authz.ErrCredentialRejected) {
			h.signOut(writer, request, authz.Principal{})
			return
		}
		if err != nil || principal.IsZero() {
			writeJSON(writer, http.StatusServiceUnavailable, errorView{Error: "authentication unavailable"})
			return
		}
		h.signOut(writer, request, principal)
	})
}
