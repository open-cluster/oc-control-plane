package webhooks

import (
	"net/http"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

// Adapter turns one Integration Type's payload into AlertEvents.
//
// It is the whole of what a provider-specific piece of this surface may be. A vendor's
// payload shape exists inside its provider package and nowhere else: nothing downstream of
// Normalise can tell which system delivered a AlertEvent, and that boundary is what makes the
// second inbound provider a bounded piece of work rather than a change to the model.
type Adapter interface {
	Authenticate(http.Header, integrations.Integration) bool
	// Normalise returns the provider identity, canonical content digest, and Alert Events.
	// Errors are permanent payload refusals; adapters must not report transient failures here.
	Normalise(body []byte) (storage.NormalizedDelivery, error)
}

type Adapters map[integrations.Provider]Adapter
