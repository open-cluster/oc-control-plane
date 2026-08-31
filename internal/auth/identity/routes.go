package identity

import (
	"net/http"

	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
)

// Base is where every route in this package hangs. Named once so a path correction is a
// one-line change rather than a search.
const Base = "/api/v1"

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
		authz.Privileged(http.MethodPost, Base+"/local-users",
			authz.MemberManage, http.HandlerFunc(h.createMember)),
		authz.Privileged(http.MethodPatch, Base+"/members/{membership}",
			authz.MemberManage, http.HandlerFunc(h.setMember)),
		authz.Privileged(http.MethodPut, Base+"/local-users/{user}/password",
			authz.MemberManage, http.HandlerFunc(h.resetLocalPassword)),
		authz.Privileged(http.MethodDelete, Base+"/members/{membership}",
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
