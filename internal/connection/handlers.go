package connection

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

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
)

// Handlers is this capability's dependencies.
type Handlers struct {
	Placements *storage.Placements
	Logger     *slog.Logger
}

// Mount registers the connection routes behind the operator surface's own authorization.
func (h Handlers) Mount(mux *http.ServeMux, authorized func(http.Handler) http.Handler) {
	mux.Handle("GET /operator/v1/organizations/{organization}/environments/{environment}/connections",
		authorized(http.HandlerFunc(h.list)))
	mux.Handle("POST /operator/v1/organizations/{organization}/environments/{environment}/connections",
		authorized(http.HandlerFunc(h.create)))
	mux.Handle("POST /operator/v1/organizations/{organization}/connections/{connection}/rotate-secret",
		authorized(http.HandlerFunc(h.rotateSecret)))
	mux.Handle("POST /operator/v1/organizations/{organization}/connections/{connection}/disable",
		authorized(http.HandlerFunc(h.disable)))
	mux.Handle("POST /operator/v1/organizations/{organization}/connections/{connection}/enable",
		authorized(http.HandlerFunc(h.enable)))
	mux.Handle("GET /operator/v1/integrations", authorized(http.HandlerFunc(h.integrations)))
}

// integrations reports what this build can be configured against, and which roles each can
// serve. It takes no organization because the vocabulary is the product's rather than a
// tenant's — there is nothing here that belongs to anyone.
func (h Handlers) integrations(writer http.ResponseWriter, _ *http.Request) {
	available := Integrations()
	views := make([]integrationView, 0, len(available))
	for _, integration := range available {
		views = append(views, integrationView{
			Integration: string(integration),
			Roles:       roleNames(offered[integration]),
		})
	}
	writeJSON(writer, http.StatusOK, integrationsView{Integrations: views})
}

func (h Handlers) list(writer http.ResponseWriter, request *http.Request) {
	organization, environment, ok := h.inEnvironment(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	list, err := h.Placements.ListConnections(ctx, organization, environment, storage.Page{
		Limit: pageSize(request),
		After: request.URL.Query().Get("after"),
	})
	if err != nil {
		h.fail(writer, request, err)
		return
	}

	h.Logger.InfoContext(ctx, "operator read connections",
		slog.String("organization", organization.String()),
		slog.String("environment_id", environment.String()),
		slog.Int("connections", len(list.Connections)),
		slog.String("caller", callerOf(request)))

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
	organization, environment, ok := h.inEnvironment(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	var body createRequest
	if !decode(writer, request, &body) {
		return
	}
	wanted, secret, ok := h.plan(writer, environment, body)
	if !ok {
		return
	}

	created, err := h.Placements.CreateConnection(ctx, organization, wanted)
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
		slog.String("caller", callerOf(request)))

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

	if err := h.Placements.RotateConnectionSecret(ctx, organization, id, Digest(secret)); err != nil {
		h.fail(writer, request, err)
		return
	}
	// Rotating a credential is as loud as issuing one. There is no overlap window in this
	// slice, so the previous secret stops working the moment this commits — which is a brief
	// outage the operator scheduled, and worth saying so they know it began.
	h.Logger.WarnContext(ctx, "operator rotated a connection secret",
		slog.String("organization", organization.String()),
		slog.String("connection_id", id.String()),
		slog.String("caller", callerOf(request)))

	writeJSON(writer, http.StatusOK, rotatedView{
		Secret: secret,
		SecretNotice: "This secret is shown once. The previous one stopped working when this " +
			"was issued; there is no overlap window.",
	})
}

func (h Handlers) disable(writer http.ResponseWriter, request *http.Request) {
	h.setDisabled(writer, request, true)
}

func (h Handlers) enable(writer http.ResponseWriter, request *http.Request) {
	h.setDisabled(writer, request, false)
}

// setDisabled turns a Connection off or back on. It is not a delete: the record of what a
// source produced survives, which is the whole reason disabling exists as a separate act.
func (h Handlers) setDisabled(writer http.ResponseWriter, request *http.Request, disabled bool) {
	organization, id, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	if err := h.Placements.SetConnectionDisabled(ctx, organization, id, disabled); err != nil {
		h.fail(writer, request, err)
		return
	}
	h.Logger.InfoContext(ctx, "operator changed a connection's disabled state",
		slog.String("organization", organization.String()),
		slog.String("connection_id", id.String()),
		slog.Bool("disabled", disabled),
		slog.String("caller", callerOf(request)))
	writer.WriteHeader(http.StatusNoContent)
}

func (h Handlers) fail(writer http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, storage.ErrUnknownOrganization):
		writeJSON(writer, http.StatusNotFound, errorView{Error: "organization not served here"})
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

func (h Handlers) inEnvironment(
	writer http.ResponseWriter, request *http.Request,
) (tenancy.Organization, uuid.UUID, bool) {
	organization, ok := h.organization(writer, request)
	if !ok {
		return tenancy.Organization{}, uuid.UUID{}, false
	}
	environment, err := uuid.Parse(request.PathValue("environment"))
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "environment is not an identity"})
		return tenancy.Organization{}, uuid.UUID{}, false
	}
	return organization, environment, true
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

func validName(writer http.ResponseWriter, name string) (string, bool) {
	trimmed := strings.TrimSpace(name)
	switch {
	case trimmed == "":
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "name must not be empty"})
		return "", false
	case len(trimmed) > maxNameLength:
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: "name must be at most " + strconv.Itoa(maxNameLength) + " characters"})
		return "", false
	}
	for _, character := range trimmed {
		if unicode.IsControl(character) {
			writeJSON(writer, http.StatusBadRequest,
				errorView{Error: "name must not contain control characters"})
			return "", false
		}
	}
	return trimmed, true
}

// validLabels bounds optional metadata. Labels are never an authorization, a credential or a
// tenant boundary and are consumed by nothing in this slice; these bounds exist so the column
// cannot become somewhere a caller stores whatever they like.
func validLabels(writer http.ResponseWriter, labels map[string]string) (map[string]string, bool) {
	if len(labels) > maxLabels {
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: "at most " + strconv.Itoa(maxLabels) + " labels"})
		return nil, false
	}
	for key, value := range labels {
		if key == "" || len(key) > maxLabelKeyLength || len(value) > maxLabelValueLength {
			writeJSON(writer, http.StatusBadRequest,
				errorView{Error: "a label key must be 1-" + strconv.Itoa(maxLabelKeyLength) +
					" characters and its value at most " + strconv.Itoa(maxLabelValueLength)})
			return nil, false
		}
	}
	return labels, true
}

func parseRole(writer http.ResponseWriter, role string) (storage.ConnectionRole, bool) {
	switch strings.TrimSpace(role) {
	case "trigger":
		return storage.RoleTrigger, true
	case "evidence":
		return storage.RoleEvidence, true
	case "both":
		return storage.RoleBoth, true
	default:
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: `role must be "trigger", "evidence" or "both"`})
		return 0, false
	}
}

func parseLocality(writer http.ResponseWriter, locality string) (storage.ExecutionLocality, bool) {
	switch strings.TrimSpace(locality) {
	case "control_plane":
		return storage.LocalityControlPlane, true
	case "relay":
		return storage.LocalityRelay, true
	default:
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: `locality must be "control_plane" or "relay"`})
		return 0, false
	}
}

func callerOf(request *http.Request) string { return request.RemoteAddr }

func pageSize(request *http.Request) int {
	size, err := strconv.Atoi(request.URL.Query().Get("limit"))
	if err != nil {
		return 0
	}
	return size
}
