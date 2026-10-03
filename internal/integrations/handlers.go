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

	"github.com/open-cluster/oc-control-plane/internal/api/listing"
	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/seal"
)

const (
	readTimeout     = 15 * time.Second
	maxNameLength   = 128
	maxRequestBytes = 16 << 10
)

type Handlers struct {
	Store        Store
	Catalog      Catalog
	WebhookTypes map[Provider]bool
	Logger       *slog.Logger
	Sealer       seal.Sealer
	PublicURL    string
}

func (h Handlers) Routes() []authz.Route {
	const base = "/api/v1"

	return []authz.Route{
		{Method: http.MethodGet, Pattern: base + "/integration-types", Permission: authz.IntegrationRead, Handler: http.HandlerFunc(h.types)},
		{Method: http.MethodGet, Pattern: base + "/integrations", Permission: authz.IntegrationRead, Handler: http.HandlerFunc(h.list)},
		{Method: http.MethodPost, Pattern: base + "/integrations", Permission: authz.IntegrationCreate, Handler: http.HandlerFunc(h.create)},
		{Method: http.MethodPost, Pattern: base + "/integration-types/{type}/connect", Permission: authz.IntegrationCreate, Handler: http.HandlerFunc(h.startConnect)},
		{Method: http.MethodGet, Pattern: CallbackPath, Handler: http.HandlerFunc(h.completeConnect)},
		{Method: http.MethodGet, Pattern: base + "/integrations/{integration}", Permission: authz.IntegrationRead, Handler: http.HandlerFunc(h.read)},
		{Method: http.MethodPatch, Pattern: base + "/integrations/{integration}", Permission: authz.IntegrationUpdate, Handler: http.HandlerFunc(h.revise)},
		{Method: http.MethodDelete, Pattern: base + "/integrations/{integration}", Permission: authz.IntegrationDelete, Handler: http.HandlerFunc(h.remove)},
		{Method: http.MethodPost, Pattern: base + "/integrations/{integration}/enable", Permission: authz.IntegrationUpdate, Handler: http.HandlerFunc(h.enable)},
		{Method: http.MethodPost, Pattern: base + "/integrations/{integration}/disable", Permission: authz.IntegrationUpdate, Handler: http.HandlerFunc(h.disable)},
		{Method: http.MethodPost, Pattern: base + "/integrations/{integration}/verify", Permission: authz.IntegrationVerify, Handler: http.HandlerFunc(h.verify)},
		{Method: http.MethodPost, Pattern: base + "/integrations/{integration}/rotate-webhook-secret", Permission: authz.IntegrationSecretRotate, Handler: http.HandlerFunc(h.rotateSecret)},
	}
}

func (h Handlers) types(writer http.ResponseWriter, request *http.Request) {
	query, err := listing.Parse(request.URL.Query(), listing.Spec{
		DefaultSort: listing.Sort{Field: "key"},
	})
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: err.Error()})
		return
	}
	principal := h.caller(request)
	organization := h.organization(request)
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	counts, err := h.Store.CountIntegrationsByProvider(ctx, principal, organization)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	configured := make(map[Provider]int, len(counts))
	for _, count := range counts {
		configured[count.Provider] = count.Count
	}

	manifests, next, err := listing.SlicePage(h.Catalog.Manifests(), query)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: err.Error()})
		return
	}
	views := make([]typeView, 0, len(manifests))
	for _, manifest := range manifests {
		definition, _ := h.Catalog.Lookup(manifest.Key)
		views = append(views, typeViewOf(definition, configured[manifest.Key], h.WebhookTypes[manifest.Key]))
	}
	writeJSON(writer, http.StatusOK, typeListView{Types: views, Next: listing.CursorPtr(next)})
}

var listSpec = listing.Spec{
	Searchable:  true,
	Sortable:    []string{"createdAt"},
	DefaultSort: listing.Sort{Field: "createdAt", Descending: true},
	Filters:     []string{"type", "relay", "disabled"},
}

func (h Handlers) list(writer http.ResponseWriter, request *http.Request) {
	principal := h.caller(request)
	organization := h.organization(request)
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
	writeJSON(writer, http.StatusOK, listing.NewPage(views, listed.Next, nil))
}

type createRequest struct {
	Type          string         `json:"type"`
	Name          string         `json:"name"`
	Configuration map[string]any `json:"configuration"`
	RelayID       string         `json:"relayId"`
}

func (h Handlers) create(writer http.ResponseWriter, request *http.Request) {
	principal := h.caller(request)
	organization := h.organization(request)
	var asked createRequest
	if !h.decode(writer, request, &asked) {
		return
	}

	definition, known := h.Catalog.Lookup(Provider(strings.TrimSpace(asked.Type)))
	if !known {
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: "type does not name an integration type this build serves"})
		return
	}
	wanted, secret, credential, refusal := h.plan(definition, asked)
	if refusal != "" {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: refusal})
		return
	}

	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	if definition.Probe != nil && !h.probeAndSeal(ctx, writer, definition, &wanted, credential) {
		return
	}

	created, err := h.Store.CreateIntegration(ctx, principal, organization, wanted)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	h.Logger.InfoContext(ctx, "integration created",
		slog.String("org_id", organization.String()),
		slog.String("integration_id", created.ID.String()),
		slog.String("type", string(definition.Key)))

	view := createdView{IntegrationView: h.viewOf(created)}
	if wanted.Verification != nil {
		view.IntegrationView.VerificationNote = wanted.Verification.Note
	}
	if secret != "" {
		view.WebhookSecret = secret
	}
	writeJSON(writer, http.StatusCreated, view)
}

func (h Handlers) plan(
	definition Definition, asked createRequest,
) (NewIntegration, string, string, string) {
	name := strings.TrimSpace(asked.Name)
	if name == "" || len(name) > maxNameLength {
		return NewIntegration{}, "", "", "name must be between 1 and 128 characters"
	}
	configuration, credential, refusal := checkConfiguration(definition, asked.Configuration, true)
	if refusal != "" {
		return NewIntegration{}, "", "", refusal
	}

	wanted := NewIntegration{
		ID:            uuid.New(),
		Provider:      definition.Key,
		Name:          name,
		Configuration: configuration,
	}

	relay := strings.TrimSpace(asked.RelayID)
	switch {
	case definition.RequiresRelay && relay == "":
		return NewIntegration{}, "", "", definition.Name + " is served through a relay; relayId is required"
	case !definition.RequiresRelay && relay != "":
		return NewIntegration{}, "", "", definition.Name + " is not served through a relay; relayId must be absent"
	case relay != "":
		id, err := uuid.Parse(relay)
		if err != nil {
			return NewIntegration{}, "", "", "relayId is not an identity"
		}
		wanted.RelayID = id
	}

	if !h.WebhookTypes[definition.Key] {
		return wanted, "", credential, ""
	}
	secret, err := GenerateSecret()
	if err != nil {
		return NewIntegration{}, "", "", "a webhook secret could not be generated; try again"
	}
	wanted.WebhookSecretDigest = Digest(secret)
	return wanted, secret, credential, ""
}

func (h Handlers) probeAndSeal(
	ctx context.Context, writer http.ResponseWriter, definition Definition,
	wanted *NewIntegration, credential string,
) bool {
	if credential != "" && !h.holdsCredentials(writer) {
		return false
	}

	verification := definition.Probe(ctx, ProbeInput{
		Integration: Integration{
			Provider:      wanted.Provider,
			Name:          wanted.Name,
			Configuration: wanted.Configuration,
		},
		Credential: credential,
	})
	if verification.Status == StatusFailed {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: verification.Note})
		return false
	}
	wanted.Verification = &verification

	if credential == "" {
		return true
	}
	sealed, ok := h.sealCredential(writer, credential, wanted.ID)
	if !ok {
		return false
	}
	wanted.CredentialSealed = sealed
	return true
}

func (h Handlers) read(writer http.ResponseWriter, request *http.Request) {
	_ = h.caller(request)
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

type reviseRequest struct {
	Name          *string        `json:"name"`
	Configuration map[string]any `json:"configuration"`
}

func (h Handlers) revise(writer http.ResponseWriter, request *http.Request) {
	principal := h.caller(request)
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

	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	credential := ""
	var definition Definition
	if asked.Configuration != nil {
		current, err := h.Store.Integration(ctx, organization, id)
		if err != nil {
			h.fail(writer, request, err)
			return
		}
		known := false
		definition, known = h.Catalog.Lookup(current.Provider)
		if !known {
			h.fail(writer, request, fmt.Errorf(
				"integration %s has provider %q this build does not serve", id, current.Provider))
			return
		}
		checked, submitted, refusal := checkConfiguration(definition, asked.Configuration, false)
		if refusal != "" {
			writeJSON(writer, http.StatusBadRequest, errorView{Error: refusal})
			return
		}
		asked.Configuration = checked
		credential = submitted

		if credential != "" {
			if !h.holdsCredentials(writer) {
				return
			}
			preview := current
			preview.Configuration = checked
			verification := definition.Probe(ctx, ProbeInput{
				Integration: preview, Credential: credential,
			})
			if verification.Status == StatusFailed {
				writeJSON(writer, http.StatusBadRequest, errorView{Error: verification.Note})
				return
			}
			sealed, ok := h.sealCredential(writer, credential, id)
			if !ok {
				return
			}

			revised, err := h.Store.ReplaceIntegrationCredential(
				ctx, principal, organization, id, Revision(asked), sealed,
				verification, nil)
			if err != nil {
				h.fail(writer, request, err)
				return
			}
			view := h.viewOf(revised)
			view.VerificationNote = verification.Note
			writeJSON(writer, http.StatusOK, view)
			return
		}
	}

	revised, err := h.Store.ReviseIntegration(ctx, principal, organization, id, Revision(asked))
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, h.viewOf(revised))
}

func (h Handlers) holdsCredentials(writer http.ResponseWriter) bool {
	if h.Sealer.Configured() {
		return true
	}
	writeJSON(writer, http.StatusServiceUnavailable, errorView{
		Error: "this deployment has no sealing key and cannot hold a credential"})
	return false
}

func (h Handlers) sealCredential(
	writer http.ResponseWriter, credential string, id uuid.UUID,
) ([]byte, bool) {
	sealed, err := h.Sealer.Seal(credential, CredentialBinding(id))
	if err == nil {
		return sealed, true
	}
	writeJSON(writer, http.StatusServiceUnavailable,
		errorView{Error: "the credential could not be stored; nothing was saved"})
	return nil, false
}

func (h Handlers) remove(writer http.ResponseWriter, request *http.Request) {
	principal := h.caller(request)
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

func (h Handlers) enable(writer http.ResponseWriter, request *http.Request) {
	h.setDisabled(writer, request, false)
}

func (h Handlers) disable(writer http.ResponseWriter, request *http.Request) {
	h.setDisabled(writer, request, true)
}

func (h Handlers) setDisabled(
	writer http.ResponseWriter, request *http.Request, disabled bool,
) {
	principal := h.caller(request)
	organization, id, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	if err := h.Store.SetIntegrationDisabled(
		ctx, principal, organization, id, disabled); err != nil {
		h.fail(writer, request, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (h Handlers) verify(writer http.ResponseWriter, request *http.Request) {
	principal := h.caller(request)
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
	definition, known := h.Catalog.Lookup(found.Provider)
	if !known {
		h.fail(writer, request, fmt.Errorf(
			"integration %s has provider %q this build does not serve", id, found.Provider))
		return
	}

	if definition.Probe != nil {
		outcome := h.probeExisting(ctx, organization, definition, found)
		verified, probeErr := h.Store.RecordIntegrationVerification(
			ctx, principal, organization, id, outcome)
		if probeErr != nil {
			h.fail(writer, request, probeErr)
			return
		}
		view := h.viewOf(verified)
		view.VerificationNote = outcome.Note
		writeJSON(writer, http.StatusOK, view)
		return
	}

	input := VerifyInput{Integration: found}
	if found.RelayID != uuid.Nil {
		status, statusErr := h.Store.IntegrationRelayStatus(ctx, organization, found.RelayID)
		if statusErr != nil {
			h.fail(writer, request, statusErr)
			return
		}
		input.RelayStatus = status
	}
	if h.WebhookTypes[definition.Key] {
		last, lastErr := h.Store.LastAcceptedDelivery(ctx, organization, id)
		if lastErr != nil {
			h.fail(writer, request, lastErr)
			return
		}
		input.LastAcceptedDelivery = last
	}

	outcome := definition.Verify(input)
	verified, err := h.Store.RecordIntegrationVerification(
		ctx, principal, organization, id, outcome)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	view := h.viewOf(verified)
	view.VerificationNote = outcome.Note
	writeJSON(writer, http.StatusOK, view)
}

func (h Handlers) probeExisting(
	ctx context.Context, organization uuid.UUID, definition Definition,
	found Integration,
) Verification {
	input := ProbeInput{Integration: found}
	if len(found.CredentialSealed) > 0 {
		if err := h.Store.RecordCredentialUnseal(
			ctx, organization, found.ID, "verification probe"); err != nil {
			return Verification{
				Status: StatusFailed,
				Note: "the credential unseal could not be recorded, so the credential " +
					"was not used; verify again once the record is writable",
			}
		}
		credential, err := h.Sealer.Open(found.CredentialSealed, CredentialBinding(found.ID))
		if err != nil {
			return Verification{
				Status: StatusFailed,
				Note: "the stored credential could not be opened by this deployment; " +
					"paste the credential again to replace it",
			}
		}
		input.Credential = credential
	}
	return definition.Probe(ctx, input)
}

func (h Handlers) rotateSecret(writer http.ResponseWriter, request *http.Request) {
	principal := h.caller(request)
	organization, id, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	secret, err := GenerateSecret()
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	if err := h.Store.RotateIntegrationWebhookSecret(
		ctx, principal, organization, id, Digest(secret)); err != nil {
		h.fail(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, rotatedView{
		WebhookSecret: secret,
		Effect:        "the previous webhook secret stopped working; there is no overlap window",
	})
}

func checkConfiguration(
	definition Definition, submitted map[string]any, requireSecret bool,
) (map[string]any, string, string) {
	credential := ""
	checked := make(map[string]any, len(submitted))
	for name, value := range submitted {
		field, declared := definition.Field(name)
		if !declared {
			return nil, "", "configuration field " + strconv.Quote(name) +
				" is not one " + definition.Name + " declares"
		}
		if field.Secret {
			pasted, isText := value.(string)
			if !isText {
				return nil, "", "configuration field " + strconv.Quote(name) + " must be text"
			}
			pasted = strings.TrimSpace(pasted)
			if err := CheckCredentialShape(pasted); err != nil {
				return nil, "", "configuration field " + strconv.Quote(name) + ": " + err.Error()
			}
			credential = pasted
			continue
		}
		if refusal := checkFieldValue(field, value); refusal != "" {
			return nil, "", refusal
		}
		checked[name] = value
	}
	for _, field := range definition.Config {
		if !field.Required {
			continue
		}
		if field.Secret {
			if requireSecret && credential == "" {
				return nil, "", "configuration field " + strconv.Quote(field.Key) + " is required"
			}
			continue
		}
		if _, present := checked[field.Key]; !present {
			return nil, "", "configuration field " + strconv.Quote(field.Key) + " is required"
		}
	}
	return checked, credential, ""
}

func checkFieldValue(field Field, value any) string {
	refuse := func() string {
		return "configuration field " + strconv.Quote(field.Key) +
			" must be a " + string(field.Type)
	}
	switch field.Type {
	case FieldString:
		text, isText := value.(string)
		if !isText {
			return refuse()
		}
		if len(field.Options) > 0 {
			for _, allowed := range field.Options {
				if text == allowed {
					return ""
				}
			}
			return "configuration field " + strconv.Quote(field.Key) +
				" must be one of its declared values"
		}
	case FieldInteger:
		number, isNumber := value.(float64)
		if !isNumber || number != float64(int64(number)) {
			return refuse()
		}
	}
	return ""
}

func (h Handlers) listQuery(
	writer http.ResponseWriter, request *http.Request,
) (Query, bool) {
	parsed, err := listing.Parse(request.URL.Query(), listSpec)
	if err != nil {
		if listing.Refused(err) {
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
		Sort:   parsed.Sort.Field, Descending: parsed.Sort.Descending,
	}
	if key := parsed.Filter("type"); key != "" {
		definition, known := h.Catalog.Lookup(Provider(key))
		if !known {
			writeJSON(writer, http.StatusBadRequest,
				errorView{Error: "type does not name an integration type this build serves"})
			return Query{}, false
		}
		query.Provider = definition.Key
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
		if named != "true" && named != "false" {
			writeJSON(writer, http.StatusBadRequest,
				errorView{Error: "disabled must be true or false"})
			return Query{}, false
		}
		disabled := named == "true"
		query.Disabled = &disabled
	}
	return query, true
}

func (h Handlers) caller(request *http.Request) authz.Principal {
	return authz.MustPrincipal(request.Context())
}

func (h Handlers) organization(request *http.Request) uuid.UUID {
	return authz.MustPrincipal(request.Context()).Organization()
}

func (h Handlers) addressed(
	writer http.ResponseWriter, request *http.Request,
) (uuid.UUID, uuid.UUID, bool) {
	organization := h.organization(request)
	id, err := uuid.Parse(request.PathValue("integration"))
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "integration is not an identity"})
		return uuid.UUID{}, uuid.UUID{}, false
	}
	return organization, id, true
}

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

func (h Handlers) fail(writer http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, authz.ErrNotAMember):
		writeJSON(writer, http.StatusNotFound, errorView{Error: "organization not found"})
	case errors.Is(err, ErrUnknown):
		writeJSON(writer, http.StatusNotFound, errorView{Error: "integration not found"})
	case errors.Is(err, ErrCrossTenant):
		writeJSON(writer, http.StatusBadRequest, errorView{Error: ErrCrossTenant.Error()})
	case errors.Is(err, ErrInUse):
		writeJSON(writer, http.StatusConflict, errorView{Error: ErrInUse.Error()})
	case errors.Is(err, ErrBadCursor):
		writeJSON(writer, http.StatusBadRequest, errorView{Error: ErrBadCursor.Error()})
	case errors.Is(err, seal.ErrNoKey):
		writeJSON(writer, http.StatusServiceUnavailable, errorView{
			Error: "this deployment has no sealing key and cannot hold a credential"})
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
