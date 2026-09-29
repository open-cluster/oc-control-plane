package webhooks

import (
	"net/http"

	"github.com/open-cluster/oc-control-plane/internal/alertevent"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// Adapter turns one Integration Type's payload into AlertEvents.
//
// It is the whole of what a provider-specific piece of this surface may be. A vendor's
// payload shape exists inside its provider package and nowhere else: nothing downstream of
// Normalize can tell which system delivered a AlertEvent, and that boundary is what makes the
// second inbound provider a bounded piece of work rather than a change to the model.
type Adapter interface {
	Authenticate(http.Header, integrations.Integration) bool
	// Normalize returns the provider identity, canonical content digest, and Alert Events.
	Normalize(body []byte) (alertevent.AlertDelivery, error)
}

type Adapters map[integrations.Provider]Adapter
