package connection

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/storage"
	"github.com/open-cluster/oc-control-plane/internal/table"
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
	// IntakeBaseURL is the public origin a customer's own system reaches intake at, for example
	// https://intake.opencluster.example. It is CONFIGURED rather than derived from the request,
	// because a URL assembled from the operator surface's own Host header would be one that
	// works from wherever the console is served and not from the customer's alerting — which is
	// the one place it has to work.
	//
	// Empty is a supported state and is served as an absence rather than a guess: a deployment
	// that has not been told where it is reachable cannot be told by this handler.
	IntakeBaseURL string
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
		authz.Privileged(http.MethodGet, base+"/connections/{connection}", authz.ConnectionRead,
			http.HandlerFunc(h.read)),
		// Revising is a PATCH because it changes part of a Connection and leaves its identity,
		// its Environment and its credential alone. A PUT would invite a client to send the whole
		// record back, and the whole record includes fields no caller may set.
		authz.Privileged(http.MethodPatch, base+"/connections/{connection}",
			authz.ConnectionUpdate, http.HandlerFunc(h.revise)),
		authz.Privileged(http.MethodDelete, base+"/connections/{connection}",
			authz.ConnectionDelete, http.HandlerFunc(h.remove)),
		authz.Privileged(http.MethodGet, base+"/connections/{connection}/dependents",
			authz.ConnectionRead, http.HandlerFunc(h.dependents)),
		authz.Privileged(http.MethodPost, base+"/connections/{connection}/enabled",
			authz.ConnectionUpdate, http.HandlerFunc(h.setEnabled)),
		authz.Privileged(http.MethodPost, base+"/connections/{connection}/validate",
			authz.ConnectionValidate, http.HandlerFunc(h.validate)),
		authz.Privileged(http.MethodGet, base+"/connections/{connection}/validations",
			authz.ConnectionRead, http.HandlerFunc(h.validations)),
		authz.Privileged(http.MethodGet, base+"/connections/{connection}/deliveries",
			authz.ConnectionRead, http.HandlerFunc(h.deliveries)),
		authz.Privileged(http.MethodPost, base+"/connections/{connection}/trigger/test-event",
			authz.ConnectionValidate, http.HandlerFunc(h.testEvent)),
		authz.Privileged(http.MethodPost, base+"/connections/{connection}/trigger/rotate-secret",
			authz.ConnectionSecretRotate, http.HandlerFunc(h.rotateSecret)),
	}
}

// catalogSpec is what the Integration catalog accepts.
//
// The default order is `lifecycle`, which means what works first and then alphabetically —
// the ordering that lets an operator find something to click without reading every tile. It is
// a sortable field rather than an implicit order so a caller can ask for the alphabetical view
// and get the same list back in a shape they chose.
var catalogSpec = table.Spec{
	Searchable:  true,
	Sortable:    []string{"lifecycle", "name", "category"},
	DefaultSort: table.Sort{Field: "lifecycle"},
	Filters:     []string{"category", "lifecycle", "role", "locality", "actionable"},
}

// integrations reports what this build names, everything a setup flow needs to render each one,
// and how many Connections of each the tenant has.
//
// The vocabulary is the product's — an Integration is a closed set compiled into the binary and
// is never a customer record — but the COUNTS are the tenant's, and they are the reason this
// route is Organization-scoped. A catalog nobody can count against is a list of what the
// product supports rather than a view of what the customer runs.
//
// It is not cursor-paginated and answers a real `total`, because the collection is compiled: it
// is thirteen records in this binary, and pretending a compiled list might not fit on a page
// would be a contract shaped by nothing.
func (h Handlers) integrations(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	query, ok := h.query(writer, request, catalogSpec)
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

	views := make([]integrationView, 0, len(definitions))
	for _, definition := range sortedCatalog(query.Sort) {
		if !matchesCatalogQuery(definition, query) {
			continue
		}
		views = append(views, catalogViewOf(definition, configured[definition.Slug]))
	}

	// The catalog pages like everything else. It is thirteen records in this binary today, and
	// it will not always be — but the reason to do it now is the contract rather than the size:
	// a listing that accepted a cursor and ignored it would hand a caller page one forever with
	// no way to tell that from having reached the end.
	matched := len(views)
	page, next, err := table.Cut(views, query)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: err.Error()})
		return
	}
	writeJSON(writer, http.StatusOK,
		table.Answer(page, next, table.Counted(matched), countPartials(list)...))
}

// countPartials says so when the per-tenant counts are short.
//
// The read behind them is bounded, so an estate past that bound would be shown a number that is
// wrong by an amount nobody can see. Saying "this column was served incompletely, and here is
// why" is what `partial` is FOR, and it is the difference between a count an operator can act on
// and one they cannot tell from a correct one.
func countPartials(list storage.ConnectionList) []table.Partial {
	if len(list.Connections) < maxCountedConnections {
		return nil
	}
	return []table.Partial{{
		Field: "configuredConnections",
		Reason: "this organization has at least " + strconv.Itoa(maxCountedConnections) +
			" connections, which is the bound on the read behind these counts; the numbers " +
			"shown are short rather than wrong in an unknown direction",
	}}
}

// sortedCatalog orders the catalog the way the caller asked. The default puts what works first,
// which is the order Definitions already renders.
func sortedCatalog(by table.Sort) []Definition {
	listed := Definitions()
	switch by.Field {
	case "name":
		sort.SliceStable(listed, func(i, j int) bool { return listed[i].Name < listed[j].Name })
	case "category":
		sort.SliceStable(listed, func(i, j int) bool {
			if listed[i].Category == listed[j].Category {
				return listed[i].Name < listed[j].Name
			}
			return listed[i].Category < listed[j].Category
		})
	}
	if by.Descending {
		slices.Reverse(listed)
	}
	return listed
}

// matchesCatalogQuery applies the narrowings a caller asked for. Each is a fact the definition
// carries, so a filter here can never disagree with what the tile renders.
func matchesCatalogQuery(definition Definition, query table.Query) bool {
	if !definition.Matches(query.Search) {
		return false
	}
	if named := query.FilterAll("category"); len(named) > 0 &&
		!slices.Contains(named, string(definition.Category)) {
		return false
	}
	if named := query.FilterAll("lifecycle"); len(named) > 0 &&
		!slices.Contains(named, string(definition.Lifecycle)) {
		return false
	}
	if named := query.Filter("actionable"); named != "" &&
		definition.Lifecycle.Actionable() != (named == "true") {
		return false
	}
	if named := query.Filter("role"); named != "" {
		role, known := roleByName(named)
		if !known || !definition.Offers(role) {
			return false
		}
	}
	if named := query.Filter("locality"); named != "" {
		locality, known := localityByName(named)
		if !known || !definition.Serves(locality) {
			return false
		}
	}
	return true
}

// connectionsSpec is what the Connections listing accepts.
//
// The Environment is a FILTER here rather than a path segment, which is the corrected shape: an
// operator asking "what does this tenant have configured" was previously asking once per scope.
// Provider, role and state join it, because a list of two hundred Connections is one an operator
// needs to narrow before they can read it (story 14).
var connectionsSpec = table.Spec{
	Searchable:  true,
	Sortable:    []string{"createdAt", "name", "integration", "state"},
	DefaultSort: table.Sort{Field: "createdAt", Descending: true},
	Filters:     []string{"environmentId", "integration", "role", "state", "disabled"},
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
	query, ok := h.query(writer, request, connectionsSpec)
	if !ok {
		return
	}
	narrowed, ok := h.narrowing(writer, query)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	list, err := h.Placements.QueryConnections(ctx, principal, organization, narrowed)
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
	// No total. Counting a cursor-paginated listing costs a second scan of everything the
	// filters matched, and `null` is how the contract spells "I did not answer this cheaply" —
	// which is worth more than a number somebody would trust.
	writeJSON(writer, http.StatusOK, table.Answer(views, list.Next, nil))
}

// narrowing turns the parsed query into what storage needs, refusing every filter value that
// could never match.
//
// A value nobody serves is REFUSED rather than narrowed to nothing, and the distinction matters
// here more than anywhere: an empty page is exactly what "this tenant has none of those" looks
// like, so a caller who wrote `evidance` would read a correct-looking answer to a question they
// did not ask.
func (h Handlers) narrowing(
	writer http.ResponseWriter, query table.Query,
) (storage.ConnectionQuery, bool) {
	narrowed := storage.ConnectionQuery{
		Page:       storage.Page{Limit: query.Limit, After: query.Cursor},
		Search:     query.Search,
		SortField:  query.Sort.Field,
		Descending: query.Sort.Descending,
	}

	if named := query.Filter("environmentId"); named != "" {
		environment, err := uuid.Parse(strings.TrimSpace(named))
		if err != nil {
			writeJSON(writer, http.StatusBadRequest,
				errorView{Error: "environmentId is not an identity"})
			return storage.ConnectionQuery{}, false
		}
		narrowed.Environment = environment
	}
	if named := query.Filter("integration"); named != "" {
		if !Known(Integration(named)) {
			writeJSON(writer, http.StatusBadRequest, errorView{
				Error: "integration " + strconv.Quote(named) + " is not one this build has"})
			return storage.ConnectionQuery{}, false
		}
		narrowed.Integration = named
	}
	if named := query.Filter("role"); named != "" {
		role, ok := roleByName(named)
		if !ok {
			writeJSON(writer, http.StatusBadRequest,
				errorView{Error: `role must be "trigger", "evidence" or "both"`})
			return storage.ConnectionQuery{}, false
		}
		narrowed.Role = role
	}
	if named := query.Filter("state"); named != "" {
		state, ok := stateByName(named)
		if !ok {
			writeJSON(writer, http.StatusBadRequest, errorView{
				Error: `state must be "configured", "validating", "active", "degraded" or "failed"`})
			return storage.ConnectionQuery{}, false
		}
		narrowed.State = state
	}
	if named := query.Filter("disabled"); named != "" {
		disabled := named == "true"
		if named != "true" && named != "false" {
			writeJSON(writer, http.StatusBadRequest,
				errorView{Error: "disabled must be true or false"})
			return storage.ConnectionQuery{}, false
		}
		narrowed.Disabled = &disabled
	}
	return narrowed, true
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
	// One question, asked in one place: may a Connection be created against this Integration at
	// all? It covers a name this build has never heard of AND a provider that is named,
	// listed in the catalog, and has no adapter. The second is the honest catalog's other half —
	// a `planned` tile that is labelled in the console and accepted by the API would be worse
	// than an unlabelled one, because the operator would get all the way to a Connection that
	// can never do anything.
	definition, refusal, ok := Configurable(integration)
	if !ok {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: refusal})
		return storage.NewConnection{}, "", false
	}

	role, ok := parseRole(writer, body.Role)
	if !ok {
		return storage.NewConnection{}, "", false
	}
	if !definition.Offers(role) {
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
	if !definition.Serves(locality) {
		// The same refusal one line up, for the other axis. A Kubernetes Connection reached
		// centrally would be a configuration whose reads have nowhere to run, and finding that
		// out at the first investigation is finding it out during an incident.
		writeJSON(writer, http.StatusBadRequest, errorView{
			Error: definition.Name + " does not run " + strconv.Quote(body.Locality) +
				"; it runs " + strings.Join(localityNames(definition), " or "),
		})
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

	configuration, ok := h.checkedConfiguration(writer, definition, body.Configuration)
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
		Configuration:     configuration,
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
	// The metadata that describes the credential without being it. The reference names this
	// database's own column, because that IS the store for a trigger secret; an opaque pointer
	// to somewhere else would be a claim about infrastructure this deployment does not have.
	wanted.Credential = storage.Credential{
		Method:      string(AuthSharedSecret),
		Reference:   triggerSecretReference,
		Fingerprint: mintFingerprint(),
	}
	return wanted, secret, true
}

// triggerSecretReference is the prefix a trigger secret's reference carries. The Connection's
// own identifier is appended by storage once the row exists, because until the insert has run
// there is no identifier to name.
const triggerSecretReference = "connection-trigger-secret"

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
		ctx, principal, organization, id, Digest(secret), mintFingerprint()); err != nil {
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

// query parses a listing's query string against what that listing serves, answering the caller
// itself on a refusal.
//
// An unknown sort or filter is REFUSED rather than ignored, and that is the whole reason this
// goes through one function. A sort silently dropped returns rows in an order nobody chose, and
// a filter silently dropped returns everything while looking narrowed — during an incident,
// both are a list that lies quietly.
func (h Handlers) query(
	writer http.ResponseWriter, request *http.Request, spec table.Spec,
) (table.Query, bool) {
	parsed, err := table.Parse(request.URL.Query(), spec)
	if err != nil {
		if table.Refused(err) {
			writeJSON(writer, http.StatusBadRequest, errorView{Error: err.Error()})
			return table.Query{}, false
		}
		// A Spec whose own default sort it does not offer. That is a programming error rather
		// than a caller's mistake, so it is logged as one and answered as one.
		h.Logger.ErrorContext(request.Context(), "a listing declares a query it cannot serve",
			slog.String("path", request.URL.Path),
			slog.String("error", err.Error()))
		writeJSON(writer, http.StatusInternalServerError, errorView{Error: "request failed"})
		return table.Query{}, false
	}
	return parsed, true
}

// localityNames renders where a provider's work may run, for the refusal that says so.
func localityNames(definition Definition) []string {
	names := make([]string, 0, len(definition.ExecutionLocalities))
	for _, locality := range definition.ExecutionLocalities {
		names = append(names, strconv.Quote(locality.String()))
	}
	return names
}
