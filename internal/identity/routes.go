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

// SCIMBase is where a customer's directory points.
//
// It is NOT under /operator/v1, and that is deliberate: an operator API version is this
// product's to change, and a directory's base URL is configured once in somebody else's system
// and then not touched for years. Pinning the provisioning surface to the SCIM standard's own
// version rather than to ours means an operator API version bump is not a change every customer
// has to make in their identity vendor.
const SCIMBase = "/scim/v2"

// scimOrganizationBase is one tenant's provisioning surface. The tenant is in the PATH as well
// as in the credential, so the same guard that authorizes every other route authorizes these —
// a surface where the tenant came only from the token would be a second, different way of
// deciding who may reach what.
const scimOrganizationBase = SCIMBase + "/organizations/{organization}"

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
		// Signing in. Nothing here is behind a credential, because obtaining one is what it is
		// for. A tenant with no provider configured answers exactly as a tenant that does not
		// exist does, so this is not a way to enumerate customers.
		authz.Public(http.MethodGet, organizationBase+"/sign-in/providers",
			http.HandlerFunc(h.signInProviders)),
		authz.Public(http.MethodGet, organizationBase+"/sign-in/{provider}",
			http.HandlerFunc(h.startSignIn)),
		authz.Public(http.MethodGet, Base+"/sign-in/callback",
			http.HandlerFunc(h.completeSignIn)),
		// SAML's assertion consumer service. It is a POST from the identity provider's own
		// redirect, so it arrives cross-site by construction and carries no credential of ours
		// — which is why it is public and why the CSRF check does not apply to it. What binds
		// it to a sign-in this product started is the single-use relay state and the request
		// identifier inside the assertion, checked together.
		authz.Public(http.MethodPost, organizationBase+"/sign-in/saml/{provider}/callback",
			http.HandlerFunc(h.completeSAMLSignIn)),

		// The caller describing themselves. Authenticated rather than privileged: requiring a
		// permission would mean an Auditor could not sign out, and a person with no membership
		// yet could not be told that they have none.
		authz.Authenticated(http.MethodGet, Base+"/session", http.HandlerFunc(h.session)),
		authz.Authenticated(http.MethodPost, Base+"/session/sign-out",
			http.HandlerFunc(h.signOut)),

		// How a tenant's people arrive.
		authz.Privileged(http.MethodGet, organizationBase+"/identity-providers",
			authz.IdentityRead, http.HandlerFunc(h.listProviders)),
		authz.Privileged(http.MethodPost, organizationBase+"/identity-providers",
			authz.IdentityConfigure, http.HandlerFunc(h.createProvider)),
		authz.Privileged(http.MethodPatch, organizationBase+"/identity-providers/{provider}",
			authz.IdentityConfigure, http.HandlerFunc(h.updateProvider)),
		authz.Privileged(http.MethodDelete, organizationBase+"/identity-providers/{provider}",
			authz.IdentityConfigure, http.HandlerFunc(h.deleteProvider)),
		// This deployment's own SAML metadata for one provider, so an administrator can hand it
		// to their identity provider rather than assembling an entity identifier and a callback
		// URL by hand. Privileged rather than public: it is read while configuring, by somebody
		// who is signed in, and a public copy would confirm which tenants use SAML.
		authz.Privileged(http.MethodGet,
			organizationBase+"/identity-providers/{provider}/saml-metadata",
			authz.IdentityRead, http.HandlerFunc(h.samlMetadata)),

		// The directory's groups as an ADMINISTRATOR sees them, and the one decision about them
		// that is theirs rather than the directory's: what each group grants here.
		authz.Privileged(http.MethodGet, organizationBase+"/directory-groups",
			authz.IdentityRead, http.HandlerFunc(h.listDirectoryGroups)),
		authz.Privileged(http.MethodPut, organizationBase+"/directory-groups/{group}/role",
			authz.IdentityConfigure, http.HandlerFunc(h.mapSCIMGroupToRole)),

		// Who they are once inside.
		authz.Privileged(http.MethodGet, organizationBase+"/members",
			authz.MemberRead, http.HandlerFunc(h.listMembers)),
		authz.Privileged(http.MethodPut, organizationBase+"/members/{user}",
			authz.MemberManage, http.HandlerFunc(h.setMember)),
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
	return append(table, h.scimRoutes()...)
}

// scimRoutes is a customer's directory writing into one tenant.
//
// Every one of them requires directory.sync and nothing else, so the credential a customer
// pastes into Okta or Entra reaches exactly this and no other part of the product. They are
// listed apart from the rest because they are a different API with a different owner: the
// shapes are RFC 7643's and the caller is a machine following RFC 7644, so they change when
// that standard does rather than when this product's surface does.
func (h Handlers) scimRoutes() authz.Table {
	provision := func(method, pattern string, handler http.HandlerFunc) authz.Route {
		return authz.Privileged(method, scimOrganizationBase+pattern,
			authz.DirectorySync, handler)
	}
	return authz.Table{
		// What a directory reads before it does anything else.
		provision(http.MethodGet, "/ServiceProviderConfig", h.scimServiceProviderConfig),
		provision(http.MethodGet, "/ResourceTypes", h.scimResourceTypes),
		provision(http.MethodGet, "/Schemas", h.scimSchemas),

		provision(http.MethodGet, "/Users", h.listSCIMUsers),
		provision(http.MethodPost, "/Users", h.createSCIMUser),
		provision(http.MethodGet, "/Users/{user}", h.readSCIMUser),
		provision(http.MethodPut, "/Users/{user}", h.replaceSCIMUser),
		provision(http.MethodPatch, "/Users/{user}", h.patchSCIMUser),
		provision(http.MethodDelete, "/Users/{user}", h.deleteSCIMUser),

		provision(http.MethodGet, "/Groups", h.listSCIMGroups),
		provision(http.MethodPost, "/Groups", h.createSCIMGroup),
		provision(http.MethodGet, "/Groups/{group}", h.readSCIMGroup),
		provision(http.MethodPut, "/Groups/{group}", h.replaceSCIMGroup),
		provision(http.MethodPatch, "/Groups/{group}", h.patchSCIMGroup),
		provision(http.MethodDelete, "/Groups/{group}", h.deleteSCIMGroup),
	}
}
