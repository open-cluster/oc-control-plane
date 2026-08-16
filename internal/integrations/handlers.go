package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/table"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

const (
	readTimeout   = 15 * time.Second
	maxNameLength = 128
	// Label bounds keep optional metadata optional: their only job is to stop the column
	// becoming somewhere a caller can put a megabyte.
	maxLabels           = 32
	maxLabelKeyLength   = 64
	maxLabelValueLength = 256
	maxRequestBytes     = 16 << 10
)

// Handlers is this capability's dependencies.
type Handlers struct {
	Store   Store
	Catalog Catalog
	Logger  *slog.Logger
	// IntakeBaseURL is the public origin a customer's own system reaches intake at. It is
	// CONFIGURED rather than derived from the request, because a URL assembled from the
	// operator surface's own Host header would work from wherever the console is served
	// and not from the customer's alerting — the one place it has to work. Empty is served
	// as an absence rather than a guess.
	IntakeBaseURL string
}

// Routes is this capability's contribution to the operator API's index.
func (h Handlers) Routes() authz.Table {
	const base = "/operator/v1/organizations/{organization}"

	return authz.Table{
		authz.Privileged(http.MethodGet, base+"/integration-types", authz.IntegrationRead,
			http.HandlerFunc(h.types)),
		authz.Privileged(http.MethodGet, base+"/integrations", authz.IntegrationRead,
			http.HandlerFunc(h.list)),
		authz.Privileged(http.MethodPost, base+"/integrations", authz.IntegrationCreate,
			http.HandlerFunc(h.create)),
		authz.Privileged(http.MethodGet, base+"/integrations/{integration}",
			authz.IntegrationRead, http.HandlerFunc(h.read)),
		authz.Privileged(http.MethodPatch, base+"/integrations/{integration}",
			authz.IntegrationUpdate, http.HandlerFunc(h.revise)),
		authz.Privileged(http.MethodDelete, base+"/integrations/{integration}",
			authz.IntegrationDelete, http.HandlerFunc(h.remove)),
		authz.Privileged(http.MethodPost, base+"/integrations/{integration}/enabled",
			authz.IntegrationUpdate, http.HandlerFunc(h.setEnabled)),
		authz.Privileged(http.MethodPost, base+"/integrations/{integration}/verify",
			authz.IntegrationVerify, http.HandlerFunc(h.verify)),
		authz.Privileged(http.MethodPost, base+"/integrations/{integration}/webhook/rotate-secret",
			authz.IntegrationSecretRotate, http.HandlerFunc(h.rotateSecret)),
	}
}

// types reports the catalog: every Integration Type this build serves, everything a setup
// flow needs to render each one, and how many the tenant has configured. The collection is
// compiled and small, so it answers whole rather than paged.
func (h Handlers) types(writer http.ResponseWriter, request *http.Request) {
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

	counts, err := h.Store.CountIntegrationsByType(ctx, principal, organization)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	configured := make(map[TypeID]int, len(counts))
	for _, count := range counts {
		configured[count.Type] = count.Count
	}

	views := make([]typeView, 0, len(h.Catalog.All()))
	for _, definition := range h.Catalog.All() {
		views = append(views, typeViewOf(definition, configured[definition.ID]))
	}
	writeJSON(writer, http.StatusOK, typeListView{Types: views})
}

// listSpec is what the integrations listing accepts, in the one table contract every
// listing on this surface speaks.
var listSpec = table.Spec{
	Searchable:  true,
	Sortable:    []string{"createdAt"},
	DefaultSort: table.Sort{Field: "createdAt", Descending: true},
	Filters:     []string{"type", "relay", "disabled"},
}

// list reports a page of the tenant's Integrations, newest first.
func (h Handlers) list(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	query, ok := h.listQuery(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	listed, err := h.Store.QueryIntegrations(ctx, principal, organization, query)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	views := make([]integrationView, 0, len(listed.Integrations))
	for _, found := range listed.Integrations {
		views = append(views, h.viewOf(found))
	}
	// No total: counting a cursor-paginated listing costs a second scan of everything the
	// filters matched, and a fabricated count is worse than an absent one.
	writeJSON(writer, http.StatusOK, table.Answer(views, listed.Next, nil))
}

// createRequest is what an operator submits.
type createRequest struct {
	// Type is the Integration Type's stable key: "alertmanager", "kubernetes".
	Type          string            `json:"type"`
	Name          string            `json:"name"`
	Configuration map[string]any    `json:"configuration"`
	Labels        map[string]string `json:"labels"`
	RelayID       string            `json:"relayId"`
}

// create records one configured installation.
func (h Handlers) create(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	var asked createRequest
	if !h.decode(writer, request, &asked) {
		return
	}

	definition, known := h.Catalog.Lookup(strings.TrimSpace(asked.Type))
	if !known {
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: "type does not name an integration type this build serves"})
		return
	}
	wanted, secret, refusal := h.plan(definition, asked, principal)
	if refusal != "" {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: refusal})
		return
	}

	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	created, err := h.Store.CreateIntegration(ctx, principal, organization, wanted)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	h.Logger.InfoContext(ctx, "integration created",
		slog.String("org_id", organization.String()),
		slog.String("integration_id", created.ID.String()),
		slog.String("type", definition.Key))

	view := createdView{IntegrationView: h.viewOf(created)}
	if secret != "" {
		// The one moment the secret exists in a response. It is not stored, not logged,
		// and no path returns it again; an operator who loses it rotates.
		view.WebhookSecret = secret
	}
	writeJSON(writer, http.StatusCreated, view)
}

// plan turns a request into what the store is asked to write, or a refusal in the
// operator's language. The webhook secret is minted here and returned beside the record,
// because after this moment it exists only as a digest.
func (h Handlers) plan(
	definition Definition, asked createRequest, principal authz.Principal,
) (NewIntegration, string, string) {
	name := strings.TrimSpace(asked.Name)
	if name == "" || len(name) > maxNameLength {
		return NewIntegration{}, "", "name must be between 1 and 128 characters"
	}
	if refusal := checkLabels(asked.Labels); refusal != "" {
		return NewIntegration{}, "", refusal
	}
	configuration, refusal := checkConfiguration(definition, asked.Configuration)
	if refusal != "" {
		return NewIntegration{}, "", refusal
	}

	wanted := NewIntegration{
		Type:          definition.ID,
		Name:          name,
		Configuration: configuration,
		Labels:        asked.Labels,
		CreatedBy:     principal.ID(),
	}

	relay := strings.TrimSpace(asked.RelayID)
	switch {
	case definition.RequiresRelay && relay == "":
		return NewIntegration{}, "", definition.Name + " is served through a relay; relayId is required"
	case !definition.RequiresRelay && relay != "":
		return NewIntegration{}, "", definition.Name + " is not served through a relay; relayId must be absent"
	case relay != "":
		id, err := uuid.Parse(relay)
		if err != nil {
			return NewIntegration{}, "", "relayId is not an identity"
		}
		wanted.RelayID = id
	}

	if !definition.ReceivesWebhooks {
		return wanted, "", ""
	}
	secret, err := GenerateSecret()
	if err != nil {
		return NewIntegration{}, "", "a webhook secret could not be generated; try again"
	}
	fingerprint, err := MintFingerprint()
	if err != nil {
		return NewIntegration{}, "", "a webhook secret could not be generated; try again"
	}
	wanted.WebhookSecretDigest = Digest(secret)
	wanted.WebhookSecretFingerprint = fingerprint
	return wanted, secret, ""
}

// read reports one Integration.
func (h Handlers) read(writer http.ResponseWriter, request *http.Request) {
	_, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, id, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	found, err := h.Store.Integration(ctx, organization, id)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, h.viewOf(found))
}

// reviseRequest is what a PATCH may change. Pointers distinguish "leave it" from "clear it".
type reviseRequest struct {
	Name          *string           `json:"name"`
	Configuration map[string]any    `json:"configuration"`
	Labels        map[string]string `json:"labels"`
}

// revise changes part of an Integration and leaves its identity, its type, its relay
// binding and its secret alone.
func (h Handlers) revise(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, id, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	var asked reviseRequest
	if !h.decode(writer, request, &asked) {
		return
	}
	if asked.Name != nil {
		trimmed := strings.TrimSpace(*asked.Name)
		if trimmed == "" || len(trimmed) > maxNameLength {
			writeJSON(writer, http.StatusBadRequest,
				errorView{Error: "name must be between 1 and 128 characters"})
			return
		}
		asked.Name = &trimmed
	}
	if asked.Labels != nil {
		if refusal := checkLabels(asked.Labels); refusal != "" {
			writeJSON(writer, http.StatusBadRequest, errorView{Error: refusal})
			return
		}
	}

	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	if asked.Configuration != nil {
		// The configuration is checked against the type's schema, which needs the type.
		current, err := h.Store.Integration(ctx, organization, id)
		if err != nil {
			h.fail(writer, request, err)
			return
		}
		definition, known := h.Catalog.ByID(current.Type)
		if !known {
			h.fail(writer, request, fmt.Errorf(
				"integration %s has type %d this build does not serve", id, current.Type))
			return
		}
		checked, refusal := checkConfiguration(definition, asked.Configuration)
		if refusal != "" {
			writeJSON(writer, http.StatusBadRequest, errorView{Error: refusal})
			return
		}
		asked.Configuration = checked
	}

	revised, err := h.Store.ReviseIntegration(ctx, principal, organization, id, Revision(asked))
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, h.viewOf(revised))
}

// remove deletes an Integration nothing depends on. One with history is refused with the
// reason; disabling is the operation for retiring a source without losing its record.
func (h Handlers) remove(writer http.ResponseWriter, request *http.Request) {
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

	if err := h.Store.DeleteIntegration(ctx, principal, organization, id); err != nil {
		h.fail(writer, request, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

// enabledRequest is idempotent on purpose: "set it to the state I want", never "toggle".
type enabledRequest struct {
	Enabled *bool `json:"enabled"`
}

func (h Handlers) setEnabled(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, id, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	var asked enabledRequest
	if !h.decode(writer, request, &asked) {
		return
	}
	if asked.Enabled == nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "enabled is required"})
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	if err := h.Store.SetIntegrationDisabled(
		ctx, principal, organization, id, !*asked.Enabled); err != nil {
		h.fail(writer, request, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

// verify checks an Integration against reality and records what it established. The
// handler gathers the observed facts; the type's own definition judges them; the store
// records the judgement. "Verified" therefore means the far end actually answered — never
// that a form was well-formed.
func (h Handlers) verify(writer http.ResponseWriter, request *http.Request) {
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

	found, err := h.Store.Integration(ctx, organization, id)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	definition, known := h.Catalog.ByID(found.Type)
	if !known {
		h.fail(writer, request, fmt.Errorf(
			"integration %s has type %d this build does not serve", id, found.Type))
		return
	}

	input := VerifyInput{Integration: found}
	if found.RelayID != uuid.Nil {
		status, statusErr := h.Store.IntegrationRelayStatus(ctx, organization, found.RelayID)
		if statusErr != nil {
			h.fail(writer, request, statusErr)
			return
		}
		input.Relay = status
	}
	if definition.ReceivesWebhooks {
		last, lastErr := h.Store.LastAcceptedDelivery(ctx, organization, id)
		if lastErr != nil {
			h.fail(writer, request, lastErr)
			return
		}
		input.LastAcceptedDelivery = last
	}

	verified, err := h.Store.RecordIntegrationVerification(
		ctx, principal, organization, id, definition.Verify(input))
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, h.viewOf(verified))
}

// rotateSecret replaces the webhook secret, returning the new value exactly once.
func (h Handlers) rotateSecret(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, id, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	secret, err := GenerateSecret()
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	fingerprint, err := MintFingerprint()
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	if err := h.Store.RotateIntegrationWebhookSecret(
		ctx, principal, organization, id, Digest(secret), fingerprint); err != nil {
		h.fail(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, rotatedView{
		WebhookSecret: secret,
		Fingerprint:   fingerprint,
		Effect:        "the previous webhook secret stopped working; there is no overlap window",
	})
}

// checkLabels bounds optional metadata.
func checkLabels(labels map[string]string) string {
	if len(labels) > maxLabels {
		return "at most " + strconv.Itoa(maxLabels) + " labels may be set"
	}
	for key, value := range labels {
		if key == "" || len(key) > maxLabelKeyLength {
			return "label keys must be between 1 and 64 characters"
		}
		if len(value) > maxLabelValueLength {
			return "label values must be at most 256 characters"
		}
	}
	return ""
}

// checkConfiguration validates submitted configuration against the type's declared fields.
// An undeclared field is refused rather than dropped, and a secret-marked field is refused
// outright: configuration never stores a credential, and accepting one silently is how a
// configuration comes to look complete and do nothing.
func checkConfiguration(
	definition Definition, submitted map[string]any,
) (map[string]any, string) {
	checked := make(map[string]any, len(submitted))
	for name, value := range submitted {
		var field Field
		declared := false
		for _, candidate := range definition.Config {
			if candidate.Name == name {
				field, declared = candidate, true
				break
			}
		}
		if !declared {
			return nil, "configuration field " + strconv.Quote(name) +
				" is not one " + definition.Name + " declares"
		}
		if field.Secret {
			return nil, "configuration field " + strconv.Quote(name) +
				" carries a credential and is not accepted here"
		}
		if refusal := checkFieldValue(field, value); refusal != "" {
			return nil, refusal
		}
		checked[name] = value
	}
	for _, field := range definition.Config {
		if field.Required {
			if _, present := checked[field.Name]; !present {
				return nil, "configuration field " + strconv.Quote(field.Name) + " is required"
			}
		}
	}
	return checked, ""
}

func checkFieldValue(field Field, value any) string {
	refuse := func() string {
		return "configuration field " + strconv.Quote(field.Name) +
			" must be a " + string(field.Type)
	}
	switch field.Type {
	case FieldString:
		text, isText := value.(string)
		if !isText {
			return refuse()
		}
		if len(field.Enum) > 0 {
			for _, allowed := range field.Enum {
				if text == allowed {
					return ""
				}
			}
			return "configuration field " + strconv.Quote(field.Name) +
				" must be one of its declared values"
		}
	case FieldInteger:
		number, isNumber := value.(float64)
		if !isNumber || number != float64(int64(number)) {
			return refuse()
		}
	case FieldBoolean:
		if _, isBool := value.(bool); !isBool {
			return refuse()
		}
	}
	return ""
}

// listQuery reads what a listing may be narrowed by, refusing anything the listing does
// not declare: a filter silently dropped returns everything while looking narrowed.
func (h Handlers) listQuery(
	writer http.ResponseWriter, request *http.Request,
) (Query, bool) {
	parsed, err := table.Parse(request.URL.Query(), listSpec)
	if err != nil {
		if table.Refused(err) {
			writeJSON(writer, http.StatusBadRequest, errorView{Error: err.Error()})
			return Query{}, false
		}
		h.Logger.ErrorContext(request.Context(), "the integrations listing declares a query it cannot serve",
			slog.String("error", err.Error()))
		writeJSON(writer, http.StatusInternalServerError, errorView{Error: "request failed"})
		return Query{}, false
	}

	query := Query{
		Page:   Page{Limit: parsed.Limit, After: parsed.Cursor},
		Search: parsed.Search,
	}
	if key := parsed.Filter("type"); key != "" {
		definition, known := h.Catalog.Lookup(key)
		if !known {
			writeJSON(writer, http.StatusBadRequest,
				errorView{Error: "type does not name an integration type this build serves"})
			return Query{}, false
		}
		query.Type = definition.ID
	}
	if named := parsed.Filter("relay"); named != "" {
		relay, err := uuid.Parse(named)
		if err != nil {
			writeJSON(writer, http.StatusBadRequest, errorView{Error: "relay is not an identity"})
			return Query{}, false
		}
		query.Relay = relay
	}
	if named := parsed.Filter("disabled"); named != "" {
		disabled := named == "true"
		query.Disabled = &disabled
	}
	return query, true
}

// caller resolves the principal the guard put on this request.
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

// organization resolves the tenant named in the path.
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

// addressed resolves the tenant and the Integration named in the path.
func (h Handlers) addressed(
	writer http.ResponseWriter, request *http.Request,
) (tenancy.Organization, uuid.UUID, bool) {
	organization, ok := h.organization(writer, request)
	if !ok {
		return tenancy.Organization{}, uuid.UUID{}, false
	}
	id, err := uuid.Parse(request.PathValue("integration"))
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "integration is not an identity"})
		return tenancy.Organization{}, uuid.UUID{}, false
	}
	return organization, id, true
}

// decode reads a bounded JSON body, refusing fields nothing declares.
func (h Handlers) decode(
	writer http.ResponseWriter, request *http.Request, into any,
) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, maxRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: "the request body is not what this operation accepts"})
		return false
	}
	return true
}

// fail answers an error, naming the ones a caller can act on.
func (h Handlers) fail(writer http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, authz.ErrNotAMember):
		// The same answer the authorization middleware gives, byte for byte. A different
		// one would confirm to a caller that a tenant they may not reach exists.
		writeJSON(writer, http.StatusNotFound, errorView{Error: "organization not found"})
	case errors.Is(err, ErrUnknown):
		writeJSON(writer, http.StatusNotFound, errorView{Error: "integration not found"})
	case errors.Is(err, ErrNameTaken):
		writeJSON(writer, http.StatusConflict, errorView{Error: ErrNameTaken.Error()})
	case errors.Is(err, ErrScope):
		writeJSON(writer, http.StatusBadRequest, errorView{Error: ErrScope.Error()})
	case errors.Is(err, ErrInUse):
		writeJSON(writer, http.StatusConflict, errorView{Error: ErrInUse.Error()})
	case errors.Is(err, ErrBadCursor):
		writeJSON(writer, http.StatusBadRequest, errorView{Error: ErrBadCursor.Error()})
	case errors.Is(err, audit.ErrWriteFailed):
		h.Logger.ErrorContext(request.Context(), "an operation was rolled back unrecorded",
			slog.String("path", request.URL.Path),
			slog.String("error", err.Error()))
		writeJSON(writer, http.StatusServiceUnavailable, errorView{
			Error: "the change was refused because it could not be recorded"})
	default:
		h.Logger.ErrorContext(request.Context(), "integration request failed",
			slog.String("path", request.URL.Path),
			slog.String("error", err.Error()))
		writeJSON(writer, http.StatusInternalServerError, errorView{Error: "request failed"})
	}
}
