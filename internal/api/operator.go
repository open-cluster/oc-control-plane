package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/identity"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/conversation"
	"github.com/open-cluster/oc-control-plane/internal/health"
	"github.com/open-cluster/oc-control-plane/internal/incident"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/postmortem"
	"github.com/open-cluster/oc-control-plane/internal/secrets"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
	"github.com/open-cluster/oc-control-plane/internal/webhooks"
)

// readTimeout bounds how long a read may take, so an operator query cannot outlive the
// attention of whoever made it.
const readTimeout = 15 * time.Second

// Handlers is the operator surface's dependencies.
type Handlers struct {
	Database                *storage.Database
	Logger                  *slog.Logger
	Identity                identity.Handlers
	Catalog                 integrations.Catalog
	Investigations          *investigation.Runner
	InvestigationWindowLead time.Duration
	Sealer                  seal.Sealer
	ConversationsEnabled    bool
	// Origins are the browser origins a cookie-authenticated unsafe request may come from.
	// Empty means no browser may make one, which is the correct posture for a deployment that
	// has not said where its console is served from.
	Origins []string
	// MaxWaitingTurns bounds one organization's unclaimed turns, so overload is a plain
	// refusal rather than a queue that grows without bound.
	MaxWaitingTurns int
	// IntakeBaseURL is the public origin a customer's own system reaches intake at. It is
	// configured rather than derived from a request
	IntakeBaseURL string
	// PublicURL is where this surface is reachable from a browser, and ConsoleURL is where
	// a browser is sent afterwards. Both are configuration for the reason IntakeBaseURL is:
	// a provider's redirect URI must be absolute and must not be assembled from a
	// caller-controlled Host header. Empty PublicURL means no provider installation flow
	// can be started, and starting one says so.
	PublicURL  string
	ConsoleURL string
	// MinimumRelayVersion is the floor the fleet summary counts `outdated` against.
	// Empty means the build states no floor.
	MinimumRelayVersion string
}

// Router returns the operator surface, or the reason it cannot be built.
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

	/* Correlation wraps the whole surface, so the identifier exists before the credential is
	   resolved and every audit event a request produces can name the log lines it produced. */
	return h.correlated(router), nil
}

// Routes is the whole operator API.
func (h Handlers) Routes() authz.Table {
	const relays = "/api/v1/relays"

	routes := authz.Table{
		authz.Authenticated(http.MethodGet, "/api/v1/meta", http.HandlerFunc(h.meta)),
		authz.Privileged(http.MethodGet, relays, authz.RelayRead,
			http.HandlerFunc(h.listRelays)),
		// The summary comes BEFORE the fleet in the table for the same reason it comes before it
		// on a page: a hundred relays is a hundred rows, and a hundred rows is not an assessment.
		authz.Privileged(http.MethodGet, relays+"/summary", authz.RelayRead,
			http.HandlerFunc(h.fleetSummary)),
		authz.Privileged(http.MethodGet, relays+"/{registration}/integrations", authz.RelayRead,
			http.HandlerFunc(h.relayIntegrations)),
		authz.Privileged(http.MethodGet, relays+"/{registration}/failures", authz.RelayRead,
			http.HandlerFunc(h.relayFailures)),
		authz.Privileged(http.MethodGet, relays+"/{registration}/session-conflicts",
			authz.RelayRead, http.HandlerFunc(h.conflictTrail)),
		// Withdrawing the mark destroys a credential-theft finding and nothing else in the
		// product records that it existed, so it is a permission of its own rather than part of
		// reading the roster — and only the Admin holds it.
		authz.Privileged(http.MethodPost, relays+"/{registration}/clear-conflict",
			authz.RelayConflictClear, http.HandlerFunc(h.clearConflict)),
		// Minting a credential that enrols a new Relay is not part of reading the fleet, so it
		// is not covered by the permission that reads it.
		authz.Privileged(http.MethodPost, relays+"/bootstrap-tokens",
			authz.RelayBootstrapIssue, http.HandlerFunc(h.issueBootstrapToken)),
	}

	routes = append(routes, h.Identity.Routes()...)
	routes = append(routes, integrations.Handlers{
		Store:         h.Database,
		Catalog:       h.Catalog,
		Logger:        h.Logger,
		Sealer:        h.Sealer,
		IntakeBaseURL: h.IntakeBaseURL,
		PublicURL:     h.PublicURL,
		ConsoleURL:    h.ConsoleURL,
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
		Store:      h.Database,
		Runner:     h.Investigations,
		Logger:     h.Logger,
		Events:     h.Database,
		WindowLead: h.InvestigationWindowLead,
	}.Routes()...)
	routes = append(routes, conversation.Handlers{
		Store:           h.Database,
		Logger:          h.Logger,
		Enabled:         h.ConversationsEnabled,
		WindowLead:      h.InvestigationWindowLead,
		MaxWaitingTurns: h.MaxWaitingTurns,
	}.Routes()...)
	routes = append(routes, webhooks.DeliveryHandlers{
		Database: h.Database,
		Logger:   h.Logger,
		Counters: webhooks.NewWorkInstruments(h.Logger),
	}.Routes()...)
	return routes
}

// meta reports stable product capabilities without exposing vendor, credential, or
// deployment configuration. It is authenticated but not Organization-scoped because the
// answer describes this composition, not tenant-owned state.
func (h Handlers) meta(writer http.ResponseWriter, _ *http.Request) {
	capabilities := []capabilityView{
		{Key: "integration_catalog", Enabled: true, Availability: "available"},
		{Key: "relay", Enabled: true, Availability: "available"},
		{Key: "webhook_delivery", Enabled: true, Availability: "available"},
		{Key: "postmortems", Enabled: true, Availability: "available"},
		{Key: "investigations", Enabled: h.Investigations != nil && h.Investigations.Investigator != nil,
			Availability: availabilityOf(h.Investigations != nil && h.Investigations.Investigator != nil)},
		{Key: "conversations", Enabled: h.ConversationsEnabled,
			Availability: availabilityOf(h.ConversationsEnabled)},
	}
	writeJSON(writer, http.StatusOK, capabilityMetadataView{Capabilities: capabilities})
}

func availabilityOf(enabled bool) string {
	if enabled {
		return "available"
	}
	return "unavailable"
}

// correlated mints a request identifier and binds it to the response and the context.
func (h Handlers) correlated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		id := newRequestID()
		writer.Header().Set(health.RequestIDHeader, id)
		next.ServeHTTP(writer, request.WithContext(
			authz.WithRequestID(request.Context(), id)))
	})
}

func newRequestID() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "uncorrelated"
	}
	return hex.EncodeToString(raw)
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

// conflictTrail reports what has happened to a relay identity.
func (h Handlers) conflictTrail(writer http.ResponseWriter, request *http.Request) {
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

	trail, err := h.Database.SessionConflictTrail(ctx, principal, organization, registration,
		storage.Page{Limit: pageSize(request), After: request.URL.Query().Get("after")})
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	h.Logger.InfoContext(ctx, "operator read a session conflict trail",
		slog.String("organization", organization.String()),
		slog.String("registration_id", registration.String()),
		slog.String("caller", h.callerName(request)))

	events := make([]conflictEventView, 0, len(trail.Events))
	for _, event := range trail.Events {
		events = append(events, eventViewOf(event))
	}
	writeJSON(writer, http.StatusOK, trailView{Events: events, Next: trail.Next})
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
		// The state asked for already holds. Nothing was written to the trail, because an act
		// that changed nothing is not part of the history of what happened.
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

// pageSize reads how many relays were asked for. An unreadable value is not an error: the
// bound is the point, and storage clamps whatever arrives.
func pageSize(request *http.Request) int {
	size, err := strconv.Atoi(request.URL.Query().Get("limit"))
	if err != nil {
		return 0
	}
	return size
}
