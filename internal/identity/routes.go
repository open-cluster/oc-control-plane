package identity

import (
	"net/http"

	"github.com/open-cluster/oc-control-plane/internal/authz"
)

// Base is where every route in this package hangs. Named once so a path correction is a
// one-line change rather than a search.
const Base = "/operator/v1"

// organizationBase is the tenant-scoped prefix. Every privileged route on this surface names
// an organization, which is what lets the guard check a membership uniformly.
const organizationBase = Base + "/organizations/{organization}"

// Routes is this capability's contribution to the operator API's index.
//
// Every entry states how it is reached by which constructor built it. Privileged takes the
// permission positionally, so a route added without one does not compile — that is the
// mechanism behind "a new route without a declared permission cannot ship", and the gate in
// internal/gates is the second half of it.
//
// The three public routes are the whole unauthenticated surface of this product, and each is
// public because a caller who is not signed in is precisely who needs it. The gate holds them
// to a named list, so a fourth fails the build until somebody records why it exists.
func (h Handlers) Routes() authz.Table {
	table := authz.Table{
		authz.Public(http.MethodPost, organizationBase+"/bootstrap",
			http.HandlerFunc(h.bootstrapLocalAdmin)),
		authz.Public(http.MethodPost, organizationBase+"/sign-in/local",
			http.HandlerFunc(h.localSignIn)),
		authz.Public(http.MethodGet, organizationBase+"/sign-in/oidc",
			http.HandlerFunc(h.startDeploymentOIDCSignIn)),
		authz.Public(http.MethodGet, Base+"/sign-in/callback",
			http.HandlerFunc(h.completeDeploymentOIDCSignIn)),
		// The caller describing themselves. Authenticated rather than privileged: requiring a
		// permission would mean an Auditor could not sign out, and a person with no membership
		// yet could not be told that they have none.
		authz.Authenticated(http.MethodGet, Base+"/session", http.HandlerFunc(h.session)),
		authz.Authenticated(http.MethodPost, Base+"/session/sign-out",
			http.HandlerFunc(h.signOut)),

		// Who they are once inside.
		authz.Privileged(http.MethodGet, organizationBase+"/members",
			authz.MemberRead, http.HandlerFunc(h.listMembers)),
		authz.Privileged(http.MethodPost, organizationBase+"/members",
			authz.MemberManage, http.HandlerFunc(h.createLocalMember)),
		authz.Privileged(http.MethodPost, organizationBase+"/members/oidc",
			authz.MemberManage, http.HandlerFunc(h.createOIDCMember)),
		authz.Privileged(http.MethodPut, organizationBase+"/members/{user}",
			authz.MemberManage, http.HandlerFunc(h.setMember)),
		authz.Privileged(http.MethodPut, organizationBase+"/members/{user}/password",
			authz.MemberManage, http.HandlerFunc(h.resetLocalPassword)),
		authz.Privileged(http.MethodDelete, organizationBase+"/members/{user}",
			authz.MemberManage, http.HandlerFunc(h.removeMember)),

		// Live sessions and their revocation.
		authz.Privileged(http.MethodGet, organizationBase+"/sessions",
			authz.MemberRead, http.HandlerFunc(h.listSessions)),
		authz.Privileged(http.MethodPost, organizationBase+"/members/{user}/revoke-sessions",
			authz.SessionRevoke, http.HandlerFunc(h.revokeSessions)),

		// The tenant's own policy.
		authz.Privileged(http.MethodGet, organizationBase+"/policy",
			authz.IdentityRead, http.HandlerFunc(h.readPolicy)),
		authz.Privileged(http.MethodPut, organizationBase+"/policy",
			authz.IdentityConfigure, http.HandlerFunc(h.writePolicy)),

		// Automation.
		authz.Privileged(http.MethodGet, organizationBase+"/service-accounts",
			authz.ServiceAccountRead, http.HandlerFunc(h.listServiceAccounts)),
		authz.Privileged(http.MethodPost, organizationBase+"/service-accounts",
			authz.ServiceAccountManage, http.HandlerFunc(h.createServiceAccount)),
		authz.Privileged(http.MethodDelete, organizationBase+"/service-accounts/{account}",
			authz.ServiceAccountManage, http.HandlerFunc(h.removeServiceAccount)),
		authz.Privileged(http.MethodGet, organizationBase+"/api-tokens",
			authz.TokenRead, http.HandlerFunc(h.listTokens)),
		authz.Privileged(http.MethodPost, organizationBase+"/api-tokens",
			authz.TokenManage, http.HandlerFunc(h.issueToken)),
		authz.Privileged(http.MethodPost, organizationBase+"/api-tokens/{token}/revoke",
			authz.TokenManage, http.HandlerFunc(h.revokeToken)),

		// The record.
		authz.Privileged(http.MethodGet, organizationBase+"/audit-events",
			authz.AuditRead, http.HandlerFunc(h.auditEvents)),
	}
	return table
}
