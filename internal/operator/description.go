package operator

import (
	"net/http"

	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/describe"
)

// WHAT THIS DEPLOYMENT SERVES, SAID OUT LOUD.
//
// A console could not learn it. Conversations are off by default and answer 404 when off,
// which is correct — a deployment with them off does not have that surface — and nothing
// said WHICH surfaces exist, so deciding whether "Investigate this incident" opens a
// Conversation or a single-shot Investigation meant probing and interpreting a 404. Every
// listing already declared its filters, its sortable fields and its default sort, and every
// write already refused a field it did not declare; none of it was served, so a client
// implemented its own dialect and its suite went green over requests this service answers
// 400.
//
// The document is DERIVED at the one place the route table is assembled, from the same
// handler values. Anything else — a file maintained beside the handlers, a generator run by
// hand — is a second copy of the contract, and the whole reason this exists is that copies
// of this contract have already drifted.

// DescriptionPath is where the document is served. It is /operator/v1 itself, which
// answered 404 until now: the index of an API is what its own base should answer.
const DescriptionPath = "/operator/v1"

// Description is what this deployment offers, assembled from the route table it is actually
// serving and what each capability said about itself.
//
// Both halves come from ONE call to capabilities(), so a description assembled from a
// different set of handler values than the ones being served is not a state this process can
// reach.
func (h Handlers) Description() describe.Document {
	routes, contributions := h.surface()
	return describe.Describe(routes, contributions)
}

// describeSelf answers the document.
//
// Authenticated rather than privileged: it describes the DEPLOYMENT and not a tenant, so
// there is no organization to check a membership in, and a caller of any role needs it to
// render anything at all. Deliberately not public — a browser that has not signed in
// discovers the sign-in providers through the identity listing that is already public, so
// nothing needs this anonymously, and publishing the route table to unauthenticated callers
// would widen the anonymous surface for no gain.
func (h Handlers) describeSelf(writer http.ResponseWriter, request *http.Request) {
	writeJSON(writer, http.StatusOK, descriptionViewOf(h.Description()))
}

// selfDescriptionRoute is the description's own entry in the table.
//
// It is built apart from surface() because surface() is what builds the description, and a
// route that appears in the table only when the description is being assembled would be a
// route the gates could not see.
func (h Handlers) selfDescriptionRoute() authz.Route {
	return authz.Authenticated(http.MethodGet, DescriptionPath,
		http.HandlerFunc(h.describeSelf))
}

// contributor is a capability that both serves routes and says what its listings and its
// request bodies accept. Every capability on this surface is one; a capability with neither
// contributes the zero value, which is a statement rather than an omission.
type contributor interface {
	Routes() authz.Table
	Describe() describe.Contribution
}
