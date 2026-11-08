package integrations

import (
	"context"
	"net/http"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
)

// ContinueConnect replaces installation-selection state with single-use OAuth state.
func (h Handlers) ContinueConnect(writer http.ResponseWriter, request *http.Request, provider Provider, providerInstallationID string) {
	principal := h.caller(request)
	ctx, cancel := context.WithTimeout(request.Context(), connectTimeout)
	defer cancel()
	flow, err := h.Store.RedeemConnectFlow(ctx, request.URL.Query().Get("state"))
	if err != nil || flow.Provider != provider || flow.ProviderInstallationID != "" ||
		flow.Principal != principal.UserID().String() || flow.Organization != principal.Organization().String() ||
		!principal.HavePermission(authz.IntegrationCreate) || !flow.ExpiresAt.After(time.Now()) {
		h.refuseConnect(writer, request, "", "installation selection could not be verified")
		return
	}
	integrationType, exists := h.Catalog.Lookup(provider)
	if !exists || !integrationType.Connectable() {
		h.refuseConnect(writer, request, flow.ReturnTo, "provider authorization is unavailable")
		return
	}
	state, err := GenerateSecret()
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	authorizationURL, err := integrationType.Connect.Authorize(ctx, state, h.callbackURL(), providerInstallationID)
	if err != nil {
		h.refuseConnect(writer, request, flow.ReturnTo, "installation selection is invalid")
		return
	}
	flow.ProviderInstallationID = providerInstallationID
	if err := h.Store.StartConnectFlow(ctx, principal.Organization(), flow, state); err != nil {
		h.fail(writer, request, err)
		return
	}
	http.Redirect(writer, request, authorizationURL, http.StatusFound)
}
