// Package identity owns who an operator is: how they sign in, which tenants they belong to,
// what automation runs as, and the record of every change to any of it.
//
// It exists because the control plane had no principal. The frontend sends a cookie session
// and the control plane required one shared static bearer token, so every browser request to a
// real control plane answered 401 — and behind that, whoever held the one token could read and
// mutate any Organization by editing a URL path segment.
//
// The identity provider is pluggable by construction. An OIDC Authorization Code flow with
// PKCE is what this build implements; SAML 2.0 and SCIM are specified, are not built here, and
// are recorded as deferred rather than silently absent. What is deliberately NOT delegated is
// the tenancy and authorization decision: memberships, roles and sessions are this control
// plane's own tables, so swapping the provider never moves the boundary.
//
// The routes live on the operator surface, which owns the listener. This package owns what
// they mean; internal/authz owns who may reach them.
//
// A deployment note that follows from the design rather than from taste: the console must be
// served from the same registrable domain as this surface. The session cookie is SameSite=Lax
// with no separate CSRF token, which is the trade the specification chose, and Lax means a
// cross-SITE console would never send the cookie at all.
package identity

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/seal"
	"github.com/open-cluster/oc-control-plane/internal/session"
	"github.com/open-cluster/oc-control-plane/internal/storage"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

const (
	// readTimeout bounds one read, so a query cannot outlive the attention of whoever made it.
	readTimeout = 15 * time.Second
	// signInTimeout is longer: completing a sign-in calls a third party's token endpoint and
	// then writes, and a timeout halfway through is worse than one that takes a moment.
	signInTimeout = 30 * time.Second
	// flowLifetime is how long a started sign-in may take to come back. Long enough for a
	// password manager, a second factor and a consent screen; short enough that an abandoned
	// attempt is not a credential lying around.
	flowLifetime = 10 * time.Minute
)

// scopes are what every authorization request asks for. Nothing beyond identity: this product
// reads a person's name and address to attribute their actions, and asking for more would be
// asking a customer's security team to approve access it does not use.
var scopes = []string{"openid", "email", "profile"}

// Bootstrap is the static credential a deployment is configured with.
//
// It is what the old shared operator token became. The difference is the whole point: it is
// bound to one Organization and one role rather than being ambient root, so a deployment that
// hands it to CI has handed out something with a stated blast radius.
//
// Its limits are worth stating plainly rather than implying. It has no expiry and no
// revocation row — revoking it means changing the file and restarting — because it exists to
// bootstrap a deployment that has no members yet. Every token issued after that comes from
// api_token, where both exist.
type Bootstrap struct {
	Digest       []byte
	Organization tenancy.Organization
	Role         authz.Role
	// Name is what the record calls this actor, so an event produced by the bootstrap
	// credential is distinguishable from one produced by a service account somebody created.
	Name string
}

// Configured reports whether a deployment set a bootstrap credential at all.
func (b Bootstrap) Configured() bool {
	return len(b.Digest) > 0 && !b.Organization.IsEmpty() && authz.KnownRole(b.Role)
}

// Handlers is this capability's dependencies.
type Handlers struct {
	Placements *storage.Placements
	Logger     *slog.Logger
	// OIDC speaks to whatever provider a tenant configured. It holds the caches, so it is one
	// value for the process rather than one per request.
	OIDC *OIDC
	// Sealer holds the key a provider's client secret is stored under. An unconfigured sealer
	// means this deployment cannot hold one, and configuring a provider is refused with that
	// reason rather than storing a secret in the clear.
	Sealer seal.Sealer
	// PublicURL is where this surface is reachable from a browser. It is what the redirect URI
	// registered with the identity provider is built from, and it is configuration rather than
	// a value read from the Host header — a caller-controlled host in a redirect URI is how an
	// authorization code is delivered somewhere else.
	PublicURL string
	// ConsoleURL is where the browser is sent once signed in.
	ConsoleURL string
	Bootstrap  Bootstrap
	// RetentionEnforced reports whether this deployment actually applies the audit retention
	// schedule a tenant declares. It is wired from the composition root rather than assumed,
	// because the policy surface states it to an auditor: a product reporting a retention period
	// it does not enforce is worse than one reporting none, and the only way to keep that
	// statement true is for the thing that starts the pruner to be the thing that says so.
	RetentionEnforced bool
}

// Resolve turns whatever a request presents into the principal holding it. It is what the
// operator surface hands internal/authz, and it is the only place this build decides that a
// credential is good.
//
// A cookie is tried before a bearer token. A browser that holds both is a browser someone has
// been experimenting in, and preferring the session means the answer matches what
// GET /operator/v1/session says about them.
func (h Handlers) Resolve(request *http.Request) (authz.Principal, error) {
	requestID := authz.RequestIDFrom(request.Context())

	if token, present := session.FromRequest(request); present {
		principal, err := h.fromSession(request, token)
		if err != nil {
			return authz.Principal{}, err
		}
		return principal.WithRequest(request.RemoteAddr, requestID), nil
	}
	if presented, present := bearerToken(request.Header.Get("Authorization")); present {
		principal, err := h.fromBearer(request, presented)
		if err != nil {
			return authz.Principal{}, err
		}
		return principal.WithRequest(request.RemoteAddr, requestID), nil
	}
	return authz.Principal{}, authz.ErrNoCredential
}

// fromSession resolves a browser session into the person holding it, with the memberships they
// hold RIGHT NOW rather than the ones they held when they signed in. That is what makes an
// administrator's revocation take effect on the colleague's next request.
func (h Handlers) fromSession(
	request *http.Request, token session.Token,
) (authz.Principal, error) {
	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	signedIn, err := h.Placements.SessionByToken(ctx, session.Digest(token))
	if err != nil {
		// The refusal says WHICH, because story 5 asks that a session which has run out
		// returns the operator to sign-in with an explanation. A session that EXPIRED and one
		// an administrator REVOKED lead to different next actions — sign in again, or ask
		// somebody why you cannot — and a bare 401 tells them neither.
		//
		// It is safe to say here: all three answers mean "you are not signed in", and none of
		// them says anything about a session the caller does not already hold. An unknown
		// cookie stays the bare rejection, so a guess at somebody else's session learns
		// nothing from being told it is not expired.
		switch {
		case errors.Is(err, session.ErrExpired):
			return authz.Principal{}, authz.Refusal{Because: authz.ReasonSessionExpired}
		case errors.Is(err, session.ErrRevoked):
			return authz.Principal{}, authz.Refusal{Because: authz.ReasonSessionRevoked}
		default:
			return authz.Principal{}, authz.ErrCredentialRejected
		}
	}

	principal, err := authz.NewPrincipal(authz.KindUser, signedIn.User.ID.String(),
		displayNameOf(signedIn.User), signedIn.Memberships)
	if err != nil {
		return authz.Principal{}, authz.ErrCredentialRejected
	}
	return principal.WithCredential(signedIn.Session.ID.String()), nil
}

// fromBearer resolves an API token. The configured bootstrap credential is compared first and
// in constant time; everything else is a row in api_token, with its own organization, role,
// expiry and revocation state.
func (h Handlers) fromBearer(
	request *http.Request, presented string,
) (authz.Principal, error) {
	digest := sha256.Sum256([]byte(presented))

	if h.Bootstrap.Configured() &&
		subtle.ConstantTimeCompare(digest[:], h.Bootstrap.Digest) == 1 {
		principal, err := authz.NewPrincipal(authz.KindServiceAccount, "bootstrap",
			h.Bootstrap.Name, []authz.Membership{{
				Organization: h.Bootstrap.Organization,
				Role:         h.Bootstrap.Role,
			}})
		if err != nil {
			return authz.Principal{}, authz.ErrCredentialRejected
		}
		return principal.WithCredential("bootstrap"), nil
	}

	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	principal, err := h.Placements.BearerPrincipal(ctx, digest[:])
	if err != nil {
		return authz.Principal{}, authz.ErrCredentialRejected
	}
	return principal, nil
}

// bearerToken pulls the credential out of an Authorization header.
func bearerToken(header string) (string, bool) {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	return token, token != ""
}

func displayNameOf(user storage.User) string {
	if name := strings.TrimSpace(user.DisplayName); name != "" {
		return name
	}
	if email := strings.TrimSpace(user.Email); email != "" {
		return email
	}
	return user.ID.String()
}
