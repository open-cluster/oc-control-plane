package connection

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/storage"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

const (
	readTimeout   = 15 * time.Second
	maxNameLength = 128
	// maxLabels and the length bounds below keep optional metadata optional. Labels are
	// consumed by nothing yet, so the only job of these numbers is to stop the column becoming
	// somewhere a caller can put a megabyte.
	maxLabels           = 32
	maxLabelKeyLength   = 64
	maxLabelValueLength = 256
	// maxCountedConnections bounds the read behind the Integration catalog's per-tenant counts.
	// A tenant past it would see a count that is short rather than a request that is slow, and
	// the number is far beyond any estate this product has met.
	maxCountedConnections = 200
)

// Handlers is this capability's dependencies.
type Handlers struct {
	Placements *storage.Placements
	Logger     *slog.Logger
}

// Routes is this capability's contribution to the operator API's index.
//
// Four paths differ from what this package served before, and each correction has a reason the
// old shape could not satisfy:
//
//   - The Integration catalog is Organization-scoped. The vocabulary itself is the product's, but
//     a console rendering it wants to say how many Connections a tenant has configured of each,
//     and a route naming no tenant cannot be counted per tenant. It is also what lets every
//     privileged route on this surface carry an organization, so the membership check is uniform.
//   - Connections are listed Organization-wide with the Environment as a FILTER rather than a path
//     segment. An operator asking "what does this tenant have configured" was previously asking
//     once per scope. The Environment a Connection belongs to is still assigned only at creation
//     and is still the sole authority for what arrives through it.
//   - One `enabled` operation with a body replaces the `enable` and `disable` pair. "Set it to the
//     state I want" is idempotent; "toggle it in the direction I named" is two operations that
//     answer differently depending on what somebody else did a second ago.
//   - Rotating the TRIGGER verification secret says so in its path. Rotating the secret a source
//     presents inbound and rotating an evidence credential are different operations, and the old
//     path claimed to be both.
func (h Handlers) Routes() authz.Table {
	const base = "/operator/v1/organizations/{organization}"

	return authz.Table{
		authz.Privileged(http.MethodGet, base+"/integrations", authz.IntegrationRead,
			http.HandlerFunc(h.integrations)),
		authz.Privileged(http.MethodGet, base+"/connections", authz.ConnectionRead,
			http.HandlerFunc(h.list)),
		authz.Privileged(http.MethodPost, base+"/connections", authz.ConnectionCreate,
			http.HandlerFunc(h.create)),
		authz.Privileged(http.MethodPost, base+"/connections/{connection}/enabled",
			authz.ConnectionUpdate, http.HandlerFunc(h.setEnabled)),
		authz.Privileged(http.MethodPost, base+"/connections/{connection}/trigger/rotate-secret",
			authz.ConnectionSecretRotate, http.HandlerFunc(h.rotateSecret)),
	}
}

// integrations reports what this build can be configured against, which roles each can serve,
// and how many Connections of each the tenant has.
//
// The vocabulary is the product's — an Integration is a closed set compiled into the binary and
// is never a customer record — but the COUNTS are the tenant's, and they are the reason this
// route is Organization-scoped. A catalog nobody can count against is a list of what the
// product supports rather than a view of what the customer runs.
func (h Handlers) integrations(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	// The catalog is the product's closed vocabulary; the counts are the tenant's. Reading them
	// together is what lets a console say "Kubernetes — 3 configured" without a second call per
	// Integration, and it is why this route is Organization-scoped.
	list, err := h.Placements.ListConnections(ctx, principal, organization, uuid.Nil, storage.Page{
		Limit: maxCountedConnections,
	})
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	configured := make(map[string]int, len(list.Connections))
	for _, found := range list.Connections {
		configured[found.Integration]++
	}

	available := Integrations()
	views := make([]integrationView, 0, len(available))
	for _, integration := range available {
		views = append(views, integrationView{
			Integration:           string(integration),
			Roles:                 roleNames(offered[integration]),
			ConfiguredConnections: configured[string(integration)],
		})
	}
	writeJSON(writer, http.StatusOK, integrationsView{Integrations: views})
}

func (h Handlers) list(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	// The Environment is a filter, and its absence means every Environment in the tenant.
	environment, ok := h.environmentFilter(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	list, err := h.Placements.ListConnections(ctx, principal, organization, environment, storage.Page{
		Limit: pageSize(request),
		After: request.URL.Query().Get("after"),
	})
	if err != nil {
		h.fail(writer, request, err)
		return
	}

	h.Logger.InfoContext(ctx, "operator read connections",
		slog.String("organization", organization.String()),
		slog.Int("connections", len(list.Connections)),
		slog.String("caller", h.callerName(request)))

	views := make([]connectionView, 0, len(list.Connections))
	for _, found := range list.Connections {
		views = append(views, viewOf(found))
	}
	writeJSON(writer, http.StatusOK, listView{Connections: views, Next: list.Next})
}

// create configures one instance of an Integration inside an Environment.
//
// The secret is minted here and returned exactly once. It is the only moment it exists in a
// readable form anywhere in this system: what is stored is a digest, no path reads it back,
// and an operator who loses it rotates rather than recovers.
func (h Handlers) create(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	var body createRequest
	if !decode(writer, request, &body) {
		return
	}
	// The Environment now arrives in the body rather than the path, because the collection is
	// Organization-wide. It is still assigned once, here, at creation, and the composite foreign
	// key still refuses one belonging to another tenant — so what changed is where the caller
	// writes it, not who decides it.
	environment, ok := h.namedEnvironment(writer, body.EnvironmentID)
	if !ok {
		return
	}
	wanted, secret, ok := h.plan(writer, environment, body)
	if !ok {
		return
	}

	created, err := h.Placements.CreateConnection(ctx, principal, organization, wanted)
	if err != nil {
		h.fail(writer, request, err)
		return
	}

	h.Logger.InfoContext(ctx, "operator created a connection",
		slog.String("organization", organization.String()),
		slog.String("environment_id", environment.String()),
		slog.String("connection_id", created.ID.String()),
		slog.String("integration", created.Integration),
		slog.String("role", created.Role.String()),
		slog.String("caller", h.callerName(request)))

	view := createdView{Connection: viewOf(created)}
	if secret != "" {
		// Shown once. It is not logged, not returned again, and not recoverable.
		view.Secret = secret
		view.SecretNotice = "This secret is shown once. Store it now; it cannot be read back."
	}
	writeJSON(writer, http.StatusCreated, view)
}

// plan turns a request into what storage needs, refusing every combination that could not
// work. It answers the caller itself on a refusal, so a handler either has a valid plan or has
// already returned.
func (h Handlers) plan(
	writer http.ResponseWriter, environment uuid.UUID, body createRequest,
) (storage.NewConnection, string, bool) {
	name, ok := validName(writer, body.Name)
	if !ok {
		return storage.NewConnection{}, "", false
	}

	integration := Integration(strings.TrimSpace(body.Integration))
	if !Known(integration) {
		writeJSON(writer, http.StatusBadRequest, errorView{
			Error: "integration " + strconv.Quote(body.Integration) + " is not one this build has",
		})
		return storage.NewConnection{}, "", false
	}

	role, ok := parseRole(writer, body.Role)
	if !ok {
		return storage.NewConnection{}, "", false
	}
	if !Offers(integration, role) {
		// Refused by the product's own knowledge of what an Integration can do, rather than
		// accepted and discovered not to work by the customer.
		writeJSON(writer, http.StatusBadRequest, errorView{
			Error: "integration " + strconv.Quote(string(integration)) +
				" does not serve the role " + strconv.Quote(body.Role),
		})
		return storage.NewConnection{}, "", false
	}

	locality, ok := parseLocality(writer, body.Locality)
	if !ok {
		return storage.NewConnection{}, "", false
	}
	relay, ok := h.relayBinding(writer, locality, body.RelayRegistrationID)
	if !ok {
		return storage.NewConnection{}, "", false
	}
	labels, ok := validLabels(writer, body.Labels)
	if !ok {
		return storage.NewConnection{}, "", false
	}

	wanted := storage.NewConnection{
		Environment:       environment,
		Integration:       string(integration),
		Name:              name,
		Role:              role,
		Locality:          locality,
		RelayRegistration: relay,
		Labels:            labels,
	}

	// Only a trigger Connection has a secret: it is presented inbound. An evidence-only
	// Connection is reached outbound and presents nothing, so minting one for it would create
	// a credential with no user.
	if !role.Includes(storage.RoleTrigger) {
		return wanted, "", true
	}
	secret, ok := h.secretFor(writer, body.Secret)
	if !ok {
		return storage.NewConnection{}, "", false
	}
	wanted.SecretDigest = Digest(secret)
	return wanted, secret, true
}

// secretFor takes the operator's secret when they supplied one and mints one when they did
// not. A supplied secret must clear the same floor a generated one clears by construction.
func (h Handlers) secretFor(writer http.ResponseWriter, supplied string) (string, bool) {
	if supplied == "" {
		generated, err := GenerateSecret()
		if err != nil {
			writeJSON(writer, http.StatusInternalServerError,
				errorView{Error: "a secret could not be generated"})
			return "", false
		}
		return generated, true
	}
	if err := CheckSecretStrength(supplied); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: err.Error()})
		return "", false
	}
	return supplied, true
}

// relayBinding resolves the Relay a Connection names, and refuses both ways of getting it
// wrong. The database enforces the same rule; refusing here means the operator is told what is
// wrong instead of receiving a constraint name.
func (h Handlers) relayBinding(
	writer http.ResponseWriter, locality storage.ExecutionLocality, named string,
) (uuid.UUID, bool) {
	named = strings.TrimSpace(named)
	if locality == storage.LocalityRelay {
		if named == "" {
			writeJSON(writer, http.StatusBadRequest, errorView{
				Error: "a relay-local connection must name the relay registration that serves it",
			})
			return uuid.Nil, false
		}
		relay, err := uuid.Parse(named)
		if err != nil {
			writeJSON(writer, http.StatusBadRequest,
				errorView{Error: "relayRegistrationId is not an identity"})
			return uuid.Nil, false
		}
		return relay, true
	}
	if named != "" {
		writeJSON(writer, http.StatusBadRequest, errorView{
			Error: "a control-plane connection is reached centrally and names no relay",
		})
		return uuid.Nil, false
	}
	return uuid.Nil, true
}

// rotateSecret replaces the digest without disturbing identity or Environment, so a suspected
// disclosure does not mean recreating the Connection and reconfiguring the source.
func (h Handlers) rotateSecret(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, id, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	var body secretRequest
	if request.ContentLength > 0 && !decode(writer, request, &body) {
		return
	}
	secret, ok := h.secretFor(writer, body.Secret)
	if !ok {
		return
	}

	if err := h.Placements.RotateConnectionSecret(
		ctx, principal, organization, id, Digest(secret)); err != nil {
		h.fail(writer, request, err)
		return
	}
	// Rotating a credential is as loud as issuing one. There is no overlap window in this
	// slice, so the previous secret stops working the moment this commits — which is a brief
	// outage the operator scheduled, and worth saying so they know it began.
	h.Logger.WarnContext(ctx, "operator rotated a connection secret",
		slog.String("organization", organization.String()),
		slog.String("connection_id", id.String()),
		slog.String("caller", h.callerName(request)))

	writeJSON(writer, http.StatusOK, rotatedView{
		Secret: secret,
		SecretNotice: "This secret is shown once. The previous one stopped working when this " +
			"was issued; there is no overlap window.",
	})
}

// setEnabled turns a Connection off or back on. It is not a delete: the record of what a source
// produced survives, which is the whole reason disabling exists as a separate act.
//
// One operation with a body rather than the enable and disable pair it replaces. "Set it to the
// state I want" is idempotent and safe to retry; "toggle it in the direction I named" answers
// differently depending on what somebody else did a second ago, which during an incident is the
// difference between a retry and an outage.
func (h Handlers) setEnabled(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, id, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	var body enabledRequest
	if !decode(writer, request, &body) {
		return
	}
	if body.Enabled == nil {
		// Refused rather than defaulted. A body with no state named is a caller who thinks they
		// said something, and guessing either direction is guessing about a Connection an
		// investigation may be reading through.
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: "enabled must be true or false"})
		return
	}

	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	if err := h.Placements.SetConnectionDisabled(
		ctx, principal, organization, id, !*body.Enabled); err != nil {
		h.fail(writer, request, err)
		return
	}
	h.Logger.InfoContext(ctx, "operator set a connection's enabled state",
		slog.String("organization", organization.String()),
		slog.String("connection_id", id.String()),
		slog.Bool("enabled", *body.Enabled),
		slog.String("caller", h.callerName(request)))
	writer.WriteHeader(http.StatusNoContent)
}

func (h Handlers) fail(writer http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, storage.ErrNotAMember), errors.Is(err, storage.ErrUnknownOrganization):
		// The same answer the authorization middleware gives. A different one here would confirm
		// to a caller that a tenant they may not reach exists.
		writeJSON(writer, http.StatusNotFound, errorView{Error: "organization not found"})
	case errors.Is(err, storage.ErrConnectionUnknown):
		writeJSON(writer, http.StatusNotFound, errorView{Error: "connection not found"})
	case errors.Is(err, storage.ErrConnectionNameTaken):
		writeJSON(writer, http.StatusConflict,
			errorView{Error: "another connection in this environment already has that name"})
	case errors.Is(err, storage.ErrConnectionScope):
		// One answer whichever half was wrong. Distinguishing "that environment is not yours"
		// from "that relay is not yours" tells a caller which half of a guess landed.
		writeJSON(writer, http.StatusNotFound,
			errorView{Error: "the environment or relay named is not in this organization"})
	case errors.Is(err, storage.ErrBadCursor):
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: "after is not a page position from a previous response"})
	case errors.Is(err, storage.ErrAuditFailed):
		// The change was possible and was rolled back because it could not be recorded. Saying
		// so is the point: an operator told "it worked" about a change nobody can attribute
		// would have exactly the gap this design refuses to leave.
		h.Logger.ErrorContext(request.Context(), "an operation was rolled back unrecorded",
			slog.String("path", request.URL.Path),
			slog.String("error", err.Error()))
		writeJSON(writer, http.StatusServiceUnavailable, errorView{
			Error: "the change was refused because it could not be recorded"})
	default:
		h.Logger.ErrorContext(request.Context(), "operator connection request failed",
			slog.String("path", request.URL.Path),
			slog.String("error", err.Error()))
		writeJSON(writer, http.StatusInternalServerError, errorView{Error: "request failed"})
	}
}

func (h Handlers) organization(
	writer http.ResponseWriter, request *http.Request,
) (tenancy.Organization, bool) {
	organization, err := tenancy.NewOrganization(request.PathValue("organization"))
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "organization is not a name"})
		return tenancy.Organization{}, false
	}
	return organization, true
}

// environmentFilter reads the optional Environment a listing is narrowed to. Absent means every
// Environment in the tenant, which is the Organization-wide read the corrected path exists for.
func (h Handlers) environmentFilter(
	writer http.ResponseWriter, request *http.Request,
) (uuid.UUID, bool) {
	named := strings.TrimSpace(request.URL.Query().Get("environmentId"))
	if named == "" {
		return uuid.Nil, true
	}
	environment, err := uuid.Parse(named)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: "environmentId is not an identity"})
		return uuid.Nil, false
	}
	return environment, true
}

// namedEnvironment reads the Environment a new Connection is being created in.
func (h Handlers) namedEnvironment(
	writer http.ResponseWriter, named string,
) (uuid.UUID, bool) {
	environment, err := uuid.Parse(strings.TrimSpace(named))
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{
			Error: "environmentId must name the environment this connection belongs to"})
		return uuid.Nil, false
	}
	return environment, true
}

func (h Handlers) addressed(
	writer http.ResponseWriter, request *http.Request,
) (tenancy.Organization, uuid.UUID, bool) {
	organization, ok := h.organization(writer, request)
	if !ok {
		return tenancy.Organization{}, uuid.UUID{}, false
	}
	id, err := uuid.Parse(request.PathValue("connection"))
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "connection is not an identity"})
		return tenancy.Organization{}, uuid.UUID{}, false
	}
	return organization, id, true
}

// callerName is who acted, for the log lines. The record itself is written by storage from the
// principal, in the transaction of the change it describes.
func (h Handlers) callerName(request *http.Request) string {
	principal, ok := authz.Of(request)
	if !ok {
		return request.RemoteAddr
	}
	return principal.DisplayName() + " (" + request.RemoteAddr + ")"
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

func pageSize(request *http.Request) int {
	size, err := strconv.Atoi(request.URL.Query().Get("limit"))
	if err != nil {
		return 0
	}
	return size
}
