package webhooks

import (
	"net/http"

	"github.com/open-cluster/oc-control-plane/internal/alertevent"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

type Adapter interface {
	Authenticate(http.Header, integrations.Integration) bool
	Normalize(body []byte) (alertevent.AlertDelivery, error)
}

type Adapters map[integrations.Provider]Adapter
