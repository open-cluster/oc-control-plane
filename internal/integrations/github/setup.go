package github

import (
	"net/http"
	"strconv"

	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

const SetupPath = "/api/v1/integration-types/github/setup"

type SetupHandlers struct{ Integrations integrations.Handlers }

func (h SetupHandlers) Routes() []authz.Route {
	return []authz.Route{{Method: http.MethodGet, Pattern: SetupPath, Permission: authz.IntegrationCreate, Handler: http.HandlerFunc(h.setup)}}
}

func (h SetupHandlers) setup(writer http.ResponseWriter, request *http.Request) {
	providerInstallationID := request.URL.Query().Get("installation_id")
	id, err := strconv.ParseInt(providerInstallationID, 10, 64)
	if err != nil || id <= 0 {
		http.Error(writer, "installation selection is invalid", http.StatusBadRequest)
		return
	}
	h.Integrations.ContinueConnect(writer, request, "github", strconv.FormatInt(id, 10))
}
