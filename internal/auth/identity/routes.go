package identity

import (
	"net/http"

	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
)

// Base is where every route in this package hangs. Named once so a path correction is a
// one-line change rather than a search.
const Base = "/api/v1"

// Routes is this capability's contribution to the operator API's index.
//
// Every entry states how it is reached by which constructor built it. Privileged takes the
// permission positionally, so a route added without one does not compile — that is the
// mechanism behind "a new route without a declared permission cannot ship", and the gate in
// test/architecture is the second half of it.
//
// The three public routes are the whole unauthenticated surface of this product, and each is
// public because a caller who is not signed in is precisely who needs it. The gate holds them
// to a named list, so a fourth fails the build until somebody records why it exists.
func (h Handlers) Routes() authz.Table {
	table := authz.Table{
		authz.Public(http.MethodPost, Base+"/auth/local/bootstrap",
			http.HandlerFunc(h.bootstrapLocalAdmin)),
		authz.Public(http.MethodPost, Base+"/auth/local/sign-in",
			http.HandlerFunc(h.localSignIn)),
		authz.Public(http.MethodGet, Base+"/auth/oidc/start",
			http.HandlerFunc(h.startDeploymentOIDCSignIn)),
		authz.Public(http.MethodGet, Base+"/auth/oidc/callback",
			http.HandlerFunc(h.completeDeploymentOIDCSignIn)),
		// The caller describing themselves. Authenticated rather than privileged: requiring a
		// permission would mean an Auditor could not sign out, and a person with no membership
		// yet could not be told that they have none.
		authz.OptionalOrganizationAuthenticated(http.MethodGet, Base+"/session",
			http.HandlerFunc(h.session)),
		authz.Authenticated(http.MethodDelete, Base+"/session",
			http.HandlerFunc(h.signOut)),
		authz.Authenticated(http.MethodGet, Base+"/organizations",
			http.HandlerFunc(h.organizations)),
		authz.Authenticated(http.MethodPost, Base+"/organizations",
			http.HandlerFunc(h.createOrganization)),
		authz.OrganizationAuthenticated(http.MethodGet, Base+"/permissions",
			http.HandlerFunc(h.permissions)),

		// Who they are once inside.
		authz.Privileged(http.MethodGet, Base+"/members",
			authz.MemberRead, http.HandlerFunc(h.listMembers)),
		authz.Privileged(http.MethodPost, Base+"/members",
			authz.MemberManage, http.HandlerFunc(h.createMember)),
		authz.Privileged(http.MethodPatch, Base+"/members/{user}",
			authz.MemberManage, http.HandlerFunc(h.setMember)),
		authz.Privileged(http.MethodPut, Base+"/members/{user}/password",
			authz.MemberManage, http.HandlerFunc(h.resetLocalPassword)),
		authz.Privileged(http.MethodDelete, Base+"/members/{user}",
			authz.MemberManage, http.HandlerFunc(h.removeMember)),

		// Live sessions and their revocation.
		authz.Privileged(http.MethodGet, Base+"/sessions",
			authz.MemberRead, http.HandlerFunc(h.listSessions)),
		authz.Privileged(http.MethodDelete, Base+"/sessions/{session}",
			authz.SessionRevoke, http.HandlerFunc(h.revokeSession)),

		// The tenant's own policy.
		authz.Privileged(http.MethodGet, Base+"/policy",
			authz.IdentityRead, http.HandlerFunc(h.readPolicy)),
		authz.Privileged(http.MethodPut, Base+"/policy",
			authz.IdentityConfigure, http.HandlerFunc(h.writePolicy)),

		// The record.
		authz.Privileged(http.MethodGet, Base+"/audit-events",
			authz.AuditRead, http.HandlerFunc(h.auditEvents)),
	}
	return table
}
