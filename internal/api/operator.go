// Package api operator serves the surface an operator uses to see and act on what the control
// plane knows about their tenant.
//
// It owns composition of the route table, and nothing else. Who a caller is comes from
// internal/auth/identity; what they may do comes from internal/auth/authz; what each route means comes
// from the route contributor that declares it. The application mounts the completed router on the
// shared HTTP listener; no second component repeats its authorization decision.
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
	Database *storage.Database
	Logger   *slog.Logger
	// Identity resolves credentials, serves the sign-in flow, and owns the identity,
	// membership, automation and audit routes.
	Identity identity.Handlers
	// Origins are the browser origins a cookie-authenticated unsafe request may come from.
	// Empty means no browser may make one, which is the correct posture for a deployment that
	// has not said where its console is served from.
	Origins []string
	// Catalog is the assembled Integration Type definitions, supplied by the composition
	// root — the only place that knows every provider.
	Catalog integrations.Catalog
	// Sealer closes over presentable credentials at rest: identity client secrets and
	// integration credentials, under the deployment's one key.
	Sealer seal.Sealer
	// Investigations runs them in the background; the investigation handlers start and
	// read through it. InvestigationWindowLead is how far before the incident began an
	// investigation's window reaches back.
	Investigations          *investigation.Runner
	InvestigationWindowLead time.Duration
	// ConversationsEnabled is the per-deployment switch for the conversation surface.
	// While it is off every conversation route answers 404 — the deployment does not have
	// that surface — and single-shot investigations are untouched.
	ConversationsEnabled bool
	// MaxWaitingTurns bounds one organization's unclaimed turns, so overload is a plain
	// refusal rather than a queue that grows without bound.
	MaxWaitingTurns int
	// IntakeBaseURL is the public origin a customer's own system reaches intake at. It is
	// configured rather than derived from a request, because a URL built from this listener's
	// own Host header would be one that works from wherever the console is served and not from
	// the customer's alerting — which is the one place it has to work. Empty is supported and is
	// served as an absence rather than as a guess.
	IntakeBaseURL string
	// PublicURL is where this surface is reachable from a browser, and ConsoleURL is where
	// a browser is sent afterwards. Both are configuration for the reason IntakeBaseURL is:
	// a provider's redirect URI must be absolute and must not be assembled from a
	// caller-controlled Host header. Empty PublicURL means no provider installation flow
	// can be started, and starting one says so.
	PublicURL  string
	ConsoleURL string
	// MinimumRelayVersion is the floor the fleet summary counts `outdated` against. Empty means
	// this build states no floor, in which case nothing is counted outdated because nothing was
	// compared — which the summary says, rather than reporting zero as though everything were
	// current.
	MinimumRelayVersion string
}

// Router returns the operator surface, or the reason it cannot be built.
//
// It is the route TABLE that is the API's index, and this function is where the table becomes a
// mux. A route that cannot be authorized correctly — an undeclared permission, a privileged
// route naming no organization, a duplicate — fails here, at startup, rather than being served
// open. The gate in test/architecture asserts the other half: that no contributor registers a route
// outside this table.
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
	// Correlation wraps the whole surface, so the identifier exists before the credential is
	// resolved and every audit event a request produces can name the log lines it produced.
	return h.correlated(router), nil
}

// Routes is the whole operator API, assembled from what each contributor declares.
//
// This package contributes the relay routes and nothing else. Every other entry comes from the
// contributor that knows what its routes mean, which is what keeps the permission a route needs
// next to the code that implements it rather than in a list somebody has to remember to edit.
func (h Handlers) Routes() authz.Table {
	return h.routesOver(h.contributors())
}

// routesOver assembles the table from contributors that have ALREADY been built, so the
// route table and the self-description can be produced from one construction of them. See
// surface: a description assembled from different handler values than the ones being served
// is the drift the description exists to end, and building them twice is how that happens.
func (h Handlers) routesOver(built []contributor) authz.Table {
	const relays = "/api/v1/organizations/{organization}/relays"

	routes := authz.Table{
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
	for _, contribution := range built {
		routes = append(routes, contribution.Routes()...)
	}
	return routes
}

// contributors builds every route-owning module this surface composes, ONCE.
//
// It exists so that the route table and the deployment's self-description are assembled from
// the same handler values. Building them twice would let the document describe a listing the
// served handler does not have — which is precisely the drift the document exists to end.
//
// The conversation handlers are built and their routes DECLARED whether or not this
// deployment serves them, so the route table — which is the API's index and what the gates
// validate — is the same table in every build. The switch lives in the handlers, which
// answer 404 while it is off; a table that changed shape with configuration would be a
// permission matrix nobody could review. What DOES change with the switch is one line of the
// self-description, which is the honest place for it.
func (h Handlers) contributors() []contributor {
	return []contributor{
		integrations.Handlers{
			Store:         h.Database,
			Catalog:       h.Catalog,
			Logger:        h.Logger,
			Sealer:        h.Sealer,
			IntakeBaseURL: h.IntakeBaseURL,
			PublicURL:     h.PublicURL,
			ConsoleURL:    h.ConsoleURL,
		},
		incident.Handlers{
			Store:  h.Database,
			Logger: h.Logger,
		},
		postmortem.Handlers{
			Service: postmortem.Service{Store: h.Database},
			Logger:  h.Logger,
		},
		investigation.Handlers{
			Store:      h.Database,
			Runner:     h.Investigations,
			Logger:     h.Logger,
			Events:     h.Database,
			WindowLead: h.InvestigationWindowLead,
		},
		conversation.Handlers{
			Store:           h.Database,
			Logger:          h.Logger,
			Enabled:         h.ConversationsEnabled,
			WindowLead:      h.InvestigationWindowLead,
			MaxWaitingTurns: h.MaxWaitingTurns,
		},
		webhooks.OperatorHandlers{Database: h.Database, Logger: h.Logger,
			Counters: webhooks.NewWorkInstruments(h.Logger)},
	}
}

// surface reports the whole route table together with what each contributor said about
// itself, from ONE construction of the contributors.
//
// That is the enforcement rather than the claim: both halves are read off the same `built`
// slice, so a description assembled from different handler values than the ones being served
// is not a state this function can produce.
type contributor interface{ Routes() authz.Table }

// correlated mints a request identifier and binds it to the response and the context.
//
// It runs before the credential is resolved, so an audit event written for a refusal can name
// the same identifier the log line for that refusal carries. A client-supplied value is not
// trusted: it would let a caller collide two unrelated requests in the record.
func (h Handlers) correlated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		id := newRequestID()
		writer.Header().Set(health.RequestIDHeader, id)
		next.ServeHTTP(writer, request.WithContext(
			authz.WithRequestID(request.Context(), id)))
	})
}

// newRequestID mints a correlation identifier. crypto/rand cannot fail in practice, and a
// request that could not be correlated is not a request worth refusing, so the fallback is a
// value that is obviously a fallback rather than an error path nobody exercises.
func newRequestID() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "uncorrelated"
	}
	return hex.EncodeToString(raw)
}

// recordRefusal writes an authorization denial to the tenant's record.
//
// It is best-effort, and the guard's own documentation says why: a denial has no operation to
// roll back, so failing the response because the record could not be written would turn an
// unreachable database into a surface that answers 500 to callers it was correctly refusing.
// The failure is logged loudly, because a refusal nobody recorded is exactly what story 22 asks
// to be visible.
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
//
// It is the answer to the question the current state cannot answer: withdrawing a finding
// destroys it, so without this the second occurrence would look exactly like the first.
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
	// Withdrawing the mark is a claim that a credential-theft finding has been dealt with, and
	// it destroys the finding, so it is recorded as loudly as the finding was — in the log here
	// and, in the same transaction as the withdrawal itself, in the audit trail.
	//
	// This line used to say the surface could report where the claim came from and never who
	// made it. It can now say both, which is what the whole slice was for.
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
