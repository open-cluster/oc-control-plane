package intake

import (
	"github.com/open-cluster/oc-control-plane/internal/connection"
	"github.com/open-cluster/oc-control-plane/internal/intake/alertmanager"
	"github.com/open-cluster/oc-control-plane/internal/storage"
)

// Adapter turns one Integration's payload into Signals.
//
// It is the whole of what an Integration-specific piece of this system may be. A vendor's
// payload shape exists inside its adapter's package and nowhere else: nothing downstream of
// Normalise can tell which system delivered a Signal, and that boundary is what makes the
// second Integration a bounded piece of work rather than a change to the model.
//
// The interface is declared here, in the package that consumes it, rather than beside the one
// implementation. That is what lets a second adapter be written without either package
// learning about the other.
//
// An error means the payload is not what this adapter accepts, and no retry will change that.
// A failure that a retry COULD fix is not an adapter's to report — it has no dependencies to
// fail — so intake maps every error from here to a permanent refusal.
type Adapter interface {
	// Normalise returns the Signals in one body and how many the source says it left out. The
	// two travel together because the second is only meaningful about the first: a count of
	// omitted alerts detached from the delivery it was omitted from says nothing.
	Normalise(body []byte) ([]storage.Signal, int, error)
}

// The adapters this build has, keyed by Integration.
//
// This is the routing half of the compiled Integration vocabulary, and internal/connection
// owns the other half — which Integrations exist and which roles each can serve. The key is
// taken from there rather than restated here, so there is one place an Integration is named
// and no way for a Connection to be configurable against something nothing can parse.
//
// A Connection naming an Integration absent from this map would be a deployment configured by
// a newer version, which intake treats as its own fault rather than the caller's. A test in
// this package proves every Integration that offers the trigger role has an adapter, so that
// case fails the build rather than a delivery.
var adapters = map[string]Adapter{
	string(connection.Alertmanager): alertmanager.Adapter{},
}

func adapterFor(integration string) (Adapter, bool) {
	adapter, ok := adapters[integration]
	return adapter, ok
}
