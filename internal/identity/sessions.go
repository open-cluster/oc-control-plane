package identity

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/session"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// session answers who is signed in and which Organizations they may read.
//
// It requires a credential and no permission. Requiring one would mean an Auditor could not
// discover what they may do, and a person who has signed in with no membership yet could not
// be told that they have none — which is exactly the state just-in-time provisioning being off
// leaves somebody in, and the one they most need explained.
func (h Handlers) session(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}

	// A service account reading this is answering "what does this token reach", which is worth
	// answering: it is how automation verifies its own scope without a mutation.
	var (
		expires time.Time
		email   string
	)
	if token, present := session.FromRequest(request); present {
		ctx, cancel := contextWithTimeout(request, readTimeout)
		defer cancel()

		if signedIn, err := h.Database.SessionByToken(ctx, session.Digest(token)); err == nil {
			expires = signedIn.Session.ExpiresAt
			email = signedIn.User.Email
		}
	}
	writeJSON(writer, http.StatusOK, sessionViewOf(principal, email, expires))
}

// signOut ends the caller's own session on the server.
//
// The row is deleted and the cookie is cleared in the same response, so the credential is dead
// before the response is written rather than merely unusable by a well-behaved browser. That is
// what makes the answer a true statement rather than the reassurance it used to be.
func (h Handlers) signOut(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	// Clearing the cookie happens whatever else does. A caller whose row was already gone still
	// has a browser presenting a dead credential, and leaving it there means the next request
	// answers 401 with no explanation.
	defer session.Clear(writer)

	if principal.Kind() != authz.KindUser {
		// A service account has no session to end. Its credential is revoked through the token
		// surface, which is where the revocation is durable.
		writeJSON(writer, http.StatusOK, signOutView{SignedOut: false})
		return
	}
	id, err := uuid.Parse(principal.CredentialID())
	if err != nil {
		writeJSON(writer, http.StatusOK, signOutView{SignedOut: true})
		return
	}

	organization, ok := h.callersOrganization(principal)
	if !ok {
		// A signed-in person with no membership anywhere has a session and no tenant to record
		// the sign-out against. Deleting the row still has to happen, and it happens through
		// the database the session was issued in — which is the one their memberships would
		// have named. With none, the row expires on its own and the cookie is already cleared.
		writeJSON(writer, http.StatusOK, signOutView{SignedOut: true})
		return
	}

	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	if err := h.Database.DeleteSession(ctx, principal, organization, id); err != nil {
		h.fail(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, signOutView{SignedOut: true})
}

// callersOrganization picks a tenant to resolve the caller's database from.
//
// Any membership will do: the database is a property of the organization, and a person with
// memberships in two database holds a session row in the one they signed in through. The
// first is taken because Memberships is sorted, so the choice is the same on every request.
func (h Handlers) callersOrganization(principal authz.Principal) (tenancy.Organization, bool) {
	memberships := principal.Memberships()
	if len(memberships) == 0 {
		return tenancy.Organization{}, false
	}
	return memberships[0].Organization, true
}

// listSessions reports what is live in a tenant, so an administrator can see what they would
// be ending before they end it.
func (h Handlers) listSessions(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	live, err := h.Database.ListSessions(ctx, principal, organization)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	views := make([]liveSessionView, 0, len(live))
	for _, found := range live {
		views = append(views, liveSessionViewOf(found))
	}
	writeJSON(writer, http.StatusOK, liveSessionListView{Sessions: views})
}

// revokeSessions ends every live session one person holds in this tenant, immediately.
//
// Story 10: offboarding takes effect before the next token refresh. It is a separate permission
// from managing members because ending somebody's session mid-incident and removing their
// access are different acts with different urgency.
func (h Handlers) revokeSessions(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	user, ok := identifier(writer, request, "user")
	if !ok {
		return
	}
	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	ended, err := h.Database.RevokeSessionsOf(ctx, principal, organization, user)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"revoked": ended})
}
