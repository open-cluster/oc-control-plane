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
	// Authorize is where the browser is sent to begin. The state is opaque to the
	// provider and must travel back unchanged; the callback is where this deployment
	// receives the return trip.
	Authorize func(state, callback string) (string, error)
	// Redeem proves that what came back belongs to whoever is standing at the provider's
	// end, and reports what to record. Refusing is the point of it: a return that proves
	// nothing must produce an error rather than an Enrolment.
	//
	// Its errors reach an operator, so their text is the PROVIDER PACKAGE'S OWN and never
	// something the vendor sent — a message composed from a vendor response is
	// attacker-influenced text on a route reached by a browser.
	Redeem func(ctx context.Context, returned ConnectReturn) (Enrolment, error)
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

// Enrolment is what a proven return says to record. It carries no credential: a type whose
// runtime access needs one seals it through the ordinary create path, and GitHub's does not
// need one at all.
type Enrolment struct {
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
