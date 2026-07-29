package intake

import "github.com/open-cluster/oc-control-plane/internal/storage"

// Adapter turns one source's payload into Signals.
//
// It is the whole of what a source-specific piece of this system may be. A vendor's payload
// shape exists inside its adapter and nowhere else: nothing downstream of Normalise can tell
// which system delivered a Signal, and that boundary is what makes the second source a bounded
// piece of work rather than a change to the model.
//
// An error means the payload is not what this adapter accepts, and no retry will change that.
// A failure that a retry COULD fix is not an adapter's to report — it has no dependencies to
// fail — so intake maps every error from here to a permanent refusal.
type Adapter interface {
	Normalise(body []byte) (Normalised, error)
}

// Normalised is one delivery's worth of alerts, plus what the source said about what it left
// out. The two travel together because the second is only meaningful about the first: a count
// of omitted alerts detached from the delivery it was omitted from says nothing.
type Normalised struct {
	Signals []storage.Signal
	// Truncated is how many alerts the source reports it did not send. Sources cap a payload
	// and say so; carrying that through is what stops a truncated delivery from being recorded
	// as a complete picture of the moment.
	Truncated int
}

// The adapters this build has. The map is the closed vocabulary the alert_source.kind column
// stores; a source naming something absent is a deployment configured by a newer version,
// which intake treats as its own fault rather than the caller's.
var adapters = map[string]Adapter{
	alertmanagerKind: alertmanagerAdapter{},
}

func adapterFor(kind string) (Adapter, bool) {
	adapter, ok := adapters[kind]
	return adapter, ok
}
