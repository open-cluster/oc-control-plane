package intake

import (
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/storage"
)

// Adapter turns one Integration Type's payload into Signals.
//
// It is the whole of what a provider-specific piece of this surface may be. A vendor's
// payload shape exists inside its provider package and nowhere else: nothing downstream of
// Normalise can tell which system delivered a Signal, and that boundary is what makes the
// second inbound provider a bounded piece of work rather than a change to the model.
//
// The interface is declared here, in the package that consumes it. Providers satisfy it
// structurally — they never import this package — and the composition root supplies the
// map, so it is the only place that knows every provider.
//
// An error means the payload is not what this adapter accepts, and no retry will change
// that. A failure a retry COULD fix is not an adapter's to report — it has no dependencies
// to fail — so intake maps every error from here to a permanent refusal.
type Adapter interface {
	// Normalise returns the Signals in one body and how many the source says it left out.
	Normalise(body []byte) ([]storage.Signal, int, error)
}

// Adapters is the routing table the composition root supplies, keyed by the Integration
// Type the payload belongs to. An Integration naming a type absent from this map is a
// deployment configured by a newer version, which intake treats as its own fault rather
// than the caller's.
type Adapters map[integrations.TypeID]Adapter
