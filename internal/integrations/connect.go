package integrations

import (
	"context"
	"errors"
	"net/url"
	"time"

	"github.com/google/uuid"
)

// CONNECTING AN INTEGRATION THROUGH THE PROVIDER'S OWN INSTALLATION FLOW.
//
// The alternative — asking a customer to install somewhere else, read an identifier out of
// a browser's address bar and paste it into a form — is not an installation flow, and it
// proves nothing: the identifier a browser carries back is chosen by whoever is driving the
// browser. Accepting it binds an account to a tenant on the strength of a number.
//
// So the flow is state-bound in both directions. Starting one records what is knowable only
// here — which organization, which principal, where the browser should land — and sends only
// a state through the browser, stored as a digest. Coming back consumes that state exactly
// once and asks the provider to prove the association before anything is recorded. The
// organization comes from the stored flow; an organization named in the callback's query is
// never read.
//
// The provider supplies the two halves that differ between vendors, and nothing else. That
// is what keeps this file free of any knowledge of GitHub.
//
// A SECOND PROVIDER ADDS NOTHING HERE. Everything below is shared, and the whole of what a
// new one writes is a Connect value on its Definition:
//
//   - Authorize: build the vendor's own installation URL, carrying the state unchanged.
//   - Redeem: prove that whoever authorized the flow can reach what the callback named,
//     and return a ConnectBinding — a suggested name and the non-secret configuration.
//     Refusing is the point of it; an unproven return must be an error.
//
// The route pair, the integration_connect_flow table, the single-use state, the principal
// check, the live probe, and recognising a repeat connection as a re-verification are all
// done for it. A provider that reaches for its own flow record, its own callback route, or
// its own duplicate check is solving a problem this file already solved — and doing it
// somewhere the security review would have to be repeated.

// ErrConnectFlowUnknown reports a state that was never issued, has expired, or has already
// been consumed. It is ONE error for all three: telling them apart is how a caller learns
// which half of a guess landed.
var ErrConnectFlowUnknown = errors.New("connect flow unknown")

// connectFlowLifetime is how long a started connect may take to come back. It is the time a
// person needs to choose an account and its repositories on the provider's own screens, and
// no longer: a state that outlived the tab it was opened in is a state worth replaying.
const connectFlowLifetime = 15 * time.Minute

// ConnectFlow is one installation flow in progress. Only its state digest travels through
// the browser; everything here is what the callback is checked against.
type ConnectFlow struct {
	ID           uuid.UUID
	Organization string
	Type         TypeID
	// Principal is the authenticated caller that started it. The callback is refused
	// unless the same one returns, so a started link handed to somebody else connects
	// nothing.
	Principal string
	// ReturnTo is the same-site path the browser lands on, already validated.
	ReturnTo  string
	ExpiresAt time.Time
}

// Connect is a provider's own installation flow, declared by the provider package. A
// definition that does not declare one is connected through its configuration form, which
// is the self-hosted path and stays supported.
type Connect struct {
	// SealsCredential declares that this flow comes back holding a runtime credential,
	// which the deployment must therefore be able to seal.
	//
	// Declared rather than discovered, because the only useful moment to act on it is
	// BEFORE the browser is sent to the vendor. A deployment with no sealing key that
	// found out on the way back would have taken a customer through choosing a workspace
	// and granting real permissions, and would then hold a live credential it can neither
	// store nor un-issue. False is GitHub's case: its runtime credential is minted from
	// the deployment's own App, so nothing here needs sealing.
	SealsCredential bool
	// Authorize is where the browser is sent to begin. The state is opaque to the
	// provider and must travel back unchanged; the callback is where this deployment
	// receives the return trip.
	Authorize func(state, callback string) (string, error)
	// Redeem proves that what came back belongs to whoever is standing at the provider's
	// end, and reports what to record. Refusing is the point of it: a return that proves
	// nothing must produce an error rather than a ConnectBinding.
	//
	// Its errors reach an operator, so their text is the PROVIDER PACKAGE'S OWN and never
	// something the vendor sent — a message composed from a vendor response is
	// attacker-influenced text on a route reached by a browser.
	Redeem func(ctx context.Context, returned ConnectReturn) (ConnectBinding, error)
}

// ConnectReturn is the callback as it arrived.
type ConnectReturn struct {
	// Query is the callback's query string. A provider reads its own parameters from it
	// and nothing else; any organization identifier here is not read by anything.
	Query url.Values
	// Callback is the same redirect URI the flow was started with, which the vendor's
	// code exchange requires to match.
	Callback string
}

// ConnectBinding is what a proven return says to record.
type ConnectBinding struct {
	// Credential is the runtime credential the flow obtained, empty when it obtained
	// none. It is sealed through the SAME path a pasted one takes — probed live first,
	// then sealed bound to the row it will live on — so there is one story about how an
	// outbound credential comes to rest, and a provider cannot invent a second.
	//
	// It exists only for the length of the callback. Nothing logs it, no view renders it,
	// and it reaches durable state only as sealed bytes.
	Credential string
	// Name is what to call the Integration if it is new, in the operator's language —
	// "GitHub — acme-corp". A name already taken is disambiguated by the handler.
	Name string
	// Configuration is what the Integration is configured with, shaped by the type's own
	// schema. It is also the identity a repeat connection is recognised by: connecting
	// the same installation again re-verifies what exists rather than duplicating it.
	Configuration map[string]any
}

// Connectable reports whether this definition offers a provider installation flow.
func (d Definition) Connectable() bool {
	return d.Connect != nil && d.Connect.Authorize != nil && d.Connect.Redeem != nil
}
