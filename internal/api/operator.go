package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/identity"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/conversation"
	"github.com/open-cluster/oc-control-plane/internal/correlation"
	"github.com/open-cluster/oc-control-plane/internal/incident"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/postmortem"
	"github.com/open-cluster/oc-control-plane/internal/seal"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
	"github.com/open-cluster/oc-control-plane/internal/webhooks"
)

const readTimeout = 15 * time.Second

type Handlers struct {
	Database                *storage.Database
	Logger                  *slog.Logger
	Identity                identity.Handlers
	Catalog                 integrations.Catalog
	WebhookTypes            map[integrations.Provider]bool
	Investigations          *investigation.Runner
	StreamContext           context.Context
	InvestigationWindowLead time.Duration
	Sealer                  seal.Sealer
	// Origins are the browser origins a cookie-authenticated unsafe request may come from.
	// Empty means no browser may make one, which is the correct posture for a deployment that
	// has not said where its console is served from.
	Origins []string
	// MaxWaitingTurns bounds one organization's unclaimed turns, so overload is a plain
	// refusal rather than a queue that grows without bound.
	MaxWaitingTurns int
	// PublicURL is where this surface is reachable from a browser and where a browser is sent
	// afterward. It is configuration because
	// a provider's redirect URI must be absolute and must not be assembled from a
	// caller-controlled Host header. Empty PublicURL means no provider installation flow
	// can be started, and starting one says so.
	PublicURL string
}

// Router returns the API surface, or the reason it cannot be built.
func (h Handlers) Router() (http.Handler, error) {
	guard := authz.Guard{
		Resolve:             h.Identity.Resolve,
		ResolveOrganization: h.Database.OrganizationExists,
		Record:              h.recordRefusal,
		Origins:             h.Origins,
		Logger:              h.Logger,
	}

	router, err := authz.Router(h.Routes(), guard)
	if err != nil {
		return nil, err
	}

	return correlation.Middleware(router), nil
}

// Routes is the whole application API.
func (h Handlers) Routes() authz.Table {
	const relays = "/api/v1/relays"

	routes := authz.Table{
		authz.Privileged(http.MethodGet, relays, authz.RelayRead,
			http.HandlerFunc(h.listRelays)),
		// The summary comes BEFORE the fleet in the table for the same reason it comes before it
		// on a page: a hundred relays is a hundred rows, and a hundred rows is not an assessment.
		authz.Privileged(http.MethodGet, relays+"/summary", authz.RelayRead,
			http.HandlerFunc(h.relaySummary)),
		authz.Privileged(http.MethodGet, relays+"/{registration}/integrations", authz.RelayRead,
			http.HandlerFunc(h.relayIntegrations)),
		authz.Privileged(http.MethodGet, relays+"/{registration}/failures", authz.RelayRead,
			http.HandlerFunc(h.relayFailures)),
		// Withdrawing the mark clears an active credential-theft finding, so it is a permission
		// of its own rather than part of reading the roster — and only the Admin holds it.
		authz.Privileged(http.MethodPost, relays+"/{registration}/clear-conflict",
			authz.RelayConflictClear, http.HandlerFunc(h.clearConflict)),
		// Minting a credential that enrols a new Relay is not part of reading the fleet, so it
		// is not covered by the permission that reads it.
		authz.Privileged(http.MethodPost, relays+"/bootstrap-tokens",
			authz.RelayBootstrapIssue, http.HandlerFunc(h.issueBootstrapToken)),
	}

	routes = append(routes, h.Identity.Routes()...)
	routes = append(routes, integrations.Handlers{
		Store:        h.Database,
		Catalog:      h.Catalog,
		WebhookTypes: h.WebhookTypes,
		Logger:       h.Logger,
		Sealer:       h.Sealer,
		PublicURL:    h.PublicURL,
	}.Routes()...)
	routes = append(routes, incident.Handlers{
		Store:  h.Database,
		Logger: h.Logger,
	}.Routes()...)
	routes = append(routes, postmortem.Handlers{
		Service: postmortem.Service{Store: h.Database},
		Logger:  h.Logger,
	}.Routes()...)
	routes = append(routes, investigation.Handlers{
		Store:         h.Database,
		Runner:        h.Investigations,
		StreamContext: h.StreamContext,
		Logger:        h.Logger,
		WindowLead:    h.InvestigationWindowLead,
		MaxPending:    h.MaxWaitingTurns,
	}.Routes()...)
	routes = append(routes, conversation.Handlers{
		Store:           h.Database,
		Logger:          h.Logger,
		WindowLead:      h.InvestigationWindowLead,
		MaxWaitingTurns: h.MaxWaitingTurns,
	}.Routes()...)
	routes = append(routes, webhooks.DeliveryHandlers{
		Database: h.Database,
		Logger:   h.Logger,
		Counters: webhooks.NewJobInstruments(h.Logger),
	}.Routes()...)
	return routes
}

// recordRefusal writes an authorization denial to the tenant's record.
func (h Handlers) recordRefusal(
	ctx context.Context, organization tenancy.Organization, event audit.Event,
) {
	if err := h.Database.RecordEvent(ctx, organization, event); err != nil {
		h.Logger.ErrorContext(ctx, "an authorization refusal could not be recorded",
			slog.String("organization", organization.String()),
			slog.String("error", err.Error()))
	}
}

// fail answers an error, naming the ones a caller can act on.
func (h Handlers) fail(writer http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, storage.ErrNotAMember), errors.Is(err, storage.ErrUnknownOrganization):
		// The same answer the authorization middleware gives, byte for byte. A different one
		// here would confirm to a caller that a tenant they may not reach exists.
		writeJSON(writer, http.StatusNotFound, errorView{Error: "organization not found"})
	case errors.Is(err, storage.ErrBadCursor), errors.Is(err, integrations.ErrBadCursor):
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: "after is not a page position from a previous response"})
	case errors.Is(err, storage.ErrAuditFailed):
		h.Logger.ErrorContext(request.Context(), "an operation was rolled back unrecorded",
			slog.String("path", request.URL.Path),
			slog.String("error", err.Error()))
		writeJSON(writer, http.StatusServiceUnavailable, errorView{
			Error: "the change was refused because it could not be recorded"})
	default:
		h.Logger.ErrorContext(request.Context(), "operator request failed",
			slog.String("path", request.URL.Path),
			slog.String("error", err.Error()))
		writeJSON(writer, http.StatusInternalServerError, errorView{Error: "request failed"})
	}
}

// clearConflict withdraws the mark on a contested relay identity.
func (h Handlers) clearConflict(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, registration, ok := h.relay(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	withdrawal, err := h.Database.ClearSessionConflict(
		ctx, principal, organization, registration)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	switch withdrawal {
	case storage.WithdrawalRelayUnknown:
		writeJSON(writer, http.StatusNotFound, errorView{Error: "relay not found"})
		return
	case storage.WithdrawalNothingMarked:
		// The state asked for already holds. Nothing is written to the audit record because an
		// act that changed nothing is not part of the history of what happened.
		writer.WriteHeader(http.StatusNoContent)
		return
	}
	h.Logger.WarnContext(ctx, "session conflict cleared by an operator",
		slog.String("organization", organization.String()),
		slog.String("registration_id", registration.String()),
		slog.String("actor", principal.ID()),
		slog.String("caller", h.callerName(request)))

	writer.WriteHeader(http.StatusNoContent)
}

// caller resolves the principal the guard put on this request. Its absence is a route mounted
// outside the permission table, which is a programming error rather than a runtime condition.
func (h Handlers) caller(
	writer http.ResponseWriter, request *http.Request,
) (authz.Principal, bool) {
	principal, ok := authz.Of(request)
	if !ok {
		h.Logger.ErrorContext(request.Context(),
			"a handler ran with no principal; the route is mounted outside the permission table",
			slog.String("path", request.URL.Path))
		writeJSON(writer, http.StatusInternalServerError, errorView{Error: "request failed"})
		return authz.Principal{}, false
	}
	return principal, true
}

// callerName is who acted, for the log lines.
func (h Handlers) callerName(request *http.Request) string {
	principal, ok := authz.Of(request)
	if !ok {
		return request.RemoteAddr
	}
	return principal.DisplayName() + " (" + request.RemoteAddr + ")"
}

// organization returns the tenant verified by the authorization middleware.
func (h Handlers) organization(
	writer http.ResponseWriter, request *http.Request,
) (tenancy.Organization, bool) {
	organization, ok := authz.ActiveOrganizationFrom(request.Context())
	if !ok {
		h.Logger.ErrorContext(request.Context(),
			"a handler ran with no verified active organization",
			slog.String("path", request.URL.Path))
		writeJSON(writer, http.StatusInternalServerError, errorView{Error: "request failed"})
		return tenancy.Organization{}, false
	}
	return organization, true
}

// relay resolves the tenant and the relay named in the path, for the routes that address one.
func (h Handlers) relay(
	writer http.ResponseWriter, request *http.Request,
) (tenancy.Organization, uuid.UUID, bool) {
	organization, ok := h.organization(writer, request)
	if !ok {
		return tenancy.Organization{}, uuid.UUID{}, false
	}
	registration, err := uuid.Parse(request.PathValue("registration"))
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "registration is not an identity"})
		return tenancy.Organization{}, uuid.UUID{}, false
	}
	return organization, registration, true
}
