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
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/secrets"
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

// Handlers is this domain surface's dependencies.
type Handlers struct {
	Store   Store
	Catalog Catalog
	Logger  *slog.Logger
	// Sealer closes over outbound credentials at rest. Unconfigured means this deployment
	// cannot hold one, and submitting a secret field is refused with that reason — never
	// stored in the clear and never silently dropped.
	Sealer seal.Sealer
	// IntakeBaseURL is the public origin a customer's own system reaches intake at. It is
	// CONFIGURED rather than derived from the request, because a URL assembled from the
	// operator surface's own Host header would work from wherever the console is served
	// and not from the customer's alerting — the one place it has to work. Empty is served
	// as an absence rather than a guess.
	IntakeBaseURL string
	// PublicURL is where this surface is reachable from a browser, and what the redirect
	// URI a provider returns an installation to is built from. Configured for the reason
	// IntakeBaseURL is: a redirect URI assembled from a request's own Host header is how
	// an authorization code is delivered somewhere else. Empty means no installation flow
	// can be started, and starting one says so.
	PublicURL string
	// ConsoleURL is where a browser is sent once an installation flow has finished. Empty
	// means the callback answers the outcome rather than redirecting to a console this
	// deployment has not been told about.
	ConsoleURL string
}

// Routes is this domain surface's contribution to the operator API's index.
func (h Handlers) Routes() authz.Table {
	const base = "/api/v1"

	return authz.Table{
		authz.Privileged(http.MethodGet, base+"/integration-types", authz.IntegrationRead,
			http.HandlerFunc(h.types)),
		authz.Privileged(http.MethodGet, base+"/integrations", authz.IntegrationRead,
			http.HandlerFunc(h.list)),
		authz.Privileged(http.MethodPost, base+"/integrations", authz.IntegrationCreate,
			http.HandlerFunc(h.create)),
		authz.Privileged(http.MethodPost, base+"/integration-types/{type}/connect",
			authz.IntegrationCreate, http.HandlerFunc(h.startConnect)),
		// The provider returns the browser HERE, to one path that names no tenant: a
		// vendor registration holds a single redirect URI, and a tenant read out of a
		// callback URL is a tenant the caller chose. What binds the return trip to an
		// organization is the single-use state redeemed against the stored flow, and
		// what binds it to a person is the credential this route still requires.
		authz.Authenticated(http.MethodGet, CallbackPath,
			http.HandlerFunc(h.completeConnect)),
		authz.Privileged(http.MethodGet, base+"/integrations/{integration}",
			authz.IntegrationRead, http.HandlerFunc(h.read)),
		authz.Privileged(http.MethodPatch, base+"/integrations/{integration}",
			authz.IntegrationUpdate, http.HandlerFunc(h.revise)),
		authz.Privileged(http.MethodDelete, base+"/integrations/{integration}",
			authz.IntegrationDelete, http.HandlerFunc(h.remove)),
		authz.Privileged(http.MethodPost, base+"/integrations/{integration}/enable",
			authz.IntegrationUpdate, http.HandlerFunc(h.enable)),
		authz.Privileged(http.MethodPost, base+"/integrations/{integration}/disable",
			authz.IntegrationUpdate, http.HandlerFunc(h.disable)),
		authz.Privileged(http.MethodPost, base+"/integrations/{integration}/verify",
			authz.IntegrationVerify, http.HandlerFunc(h.verify)),
		authz.Privileged(http.MethodPost, base+"/integrations/{integration}/rotate-webhook-secret",
			authz.IntegrationSecretRotate, http.HandlerFunc(h.rotateSecret)),
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

	manifests, next, err := listing.Cut(h.Catalog.Manifests(), query)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: err.Error()})
		return
	}
	views := make([]typeView, 0, len(manifests))
	for _, manifest := range manifests {
		views = append(views, typeViewOf(manifest, configured[manifest.ID]))
	}
	writeJSON(writer, http.StatusOK, typeListView{Types: views, Next: listing.Continuation(next)})
}

var listSpec = listing.Spec{
	Searchable:  true,
	Sortable:    []string{"createdAt"},
	DefaultSort: listing.Sort{Field: "createdAt", Descending: true},
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
	writeJSON(writer, http.StatusOK, listing.Answer(views, listed.Next, nil))
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
	wanted, secret, credential, refusal := h.plan(definition, asked, principal)
	if refusal != "" {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: refusal})
		return
	}

	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	// An outbound type is verified against the real provider BEFORE anything is stored:
	// a typo fails here, at setup, not during the next incident. The probe's judgement
	// travels into the create, so the Integration is born verified in the same
	// transaction that records it.
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
		slog.String("type", definition.Key))

	view := createdView{IntegrationView: h.viewOf(created)}
	if secret != "" {
		// The one moment the secret exists in a response. It is not stored, not logged,
		// and no path returns it again; an operator who loses it rotates.
		view.WebhookSecret = secret
	}
	writeJSON(writer, http.StatusCreated, view)
}

// plan turns a request into what the store is asked to write, plus the two values that
// exist only in this moment: the minted webhook secret, which after this exists only as a
// digest, and the pasted credential, which after the probe exists only sealed. A refusal
// is in the operator's language.
func (h Handlers) plan(
	definition Definition, asked createRequest, principal authz.Principal,
) (NewIntegration, string, string, string) {
	name := strings.TrimSpace(asked.Name)
	if name == "" || len(name) > maxNameLength {
		return NewIntegration{}, "", "", "name must be between 1 and 128 characters"
	}
	if refusal := checkLabels(asked.Labels); refusal != "" {
		return NewIntegration{}, "", "", refusal
	}
	configuration, credential, refusal := checkConfiguration(definition, asked.Configuration, true)
	if refusal != "" {
		return NewIntegration{}, "", "", refusal
	}

	wanted := NewIntegration{
		// Minted here, before the probe seals anything, so the sealed credential can
		// bind to the row it will live on.
		ID:            uuid.New(),
		Type:          definition.ID,
		Name:          name,
		Configuration: configuration,
		Labels:        asked.Labels,
		CreatedBy:     principal.ID(),
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

	if !definition.ReceivesWebhooks {
		return wanted, "", credential, ""
	}
	secret, err := GenerateSecret()
	if err != nil {
		return NewIntegration{}, "", "", "a webhook secret could not be generated; try again"
	}
	fingerprint, err := MintFingerprint()
	if err != nil {
		return NewIntegration{}, "", "", "a webhook secret could not be generated; try again"
	}
	wanted.WebhookSecretDigest = Digest(secret)
	wanted.WebhookSecretFingerprint = fingerprint
	return wanted, secret, credential, ""
}

// probeAndSeal verifies outbound reality before anything is stored: the definition's
// probe is given the installation as it is about to exist, a failed judgement refuses the
// create with the note as the reason, and only then is the credential sealed. The probe
// runs before the seal so a credential the provider refused is never stored at all.
func (h Handlers) probeAndSeal(
	ctx context.Context, writer http.ResponseWriter, definition Definition,
	wanted *NewIntegration, credential string,
) bool {
	if credential != "" && !h.holdsCredentials(writer) {
		return false
	}

	verification := definition.Probe(ctx, ProbeInput{
		Integration: Integration{
			Type:          wanted.Type,
			Name:          wanted.Name,
			Configuration: wanted.Configuration,
			Labels:        wanted.Labels,
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
	sealed, fingerprint, ok := h.sealCredential(writer, credential, wanted.ID)
	if !ok {
		return false
	}
	wanted.CredentialSealed = sealed
	wanted.CredentialFingerprint = fingerprint
	return true
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

	credential := ""
	var definition Definition
	if asked.Configuration != nil {
		// The configuration is checked against the type's schema, which needs the type.
		current, err := h.Store.Integration(ctx, organization, id)
		if err != nil {
			h.fail(writer, request, err)
			return
		}
		known := false
		definition, known = h.Catalog.ByID(current.Type)
		if !known {
			h.fail(writer, request, fmt.Errorf(
				"integration %s has type %d this build does not serve", id, current.Type))
			return
		}
		// A secret field may be absent here, unlike at creation: absence means "keep the
		// credential I already gave you", which is what write-only after entry implies.
		checked, submitted, refusal := checkConfiguration(definition, asked.Configuration, false)
		if refusal != "" {
			writeJSON(writer, http.StatusBadRequest, errorView{Error: refusal})
			return
		}
		asked.Configuration = checked
		credential = submitted

		if credential != "" {
			// The replacement is probed and sealed BEFORE anything is applied, and the
			// store applies revision and credential in one transaction — so a pasted-wrong
			// token, a missing sealing key or a name conflict each change nothing at all.
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
			sealed, fingerprint, ok := h.sealCredential(writer, credential, id)
			if !ok {
				return
			}

			// nil installation: a credential typed into the configuration form names no
			// vendor installation, so there is no routing record to refresh.
			revised, err := h.Store.ReplaceIntegrationCredential(
				ctx, principal, organization, id, Revision(asked), sealed, fingerprint,
				verification, nil)
			if err != nil {
				h.fail(writer, request, err)
				return
			}
			writeJSON(writer, http.StatusOK, h.viewOf(revised))
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

// holdsCredentials answers whether this deployment can store a pasted credential at all,
// refusing in the operator's language when it cannot. Checked before the provider is
// probed, so a deployment that could not store the answer never spends the vendor call.
func (h Handlers) holdsCredentials(writer http.ResponseWriter) bool {
	if h.Sealer.Configured() {
		return true
	}
	writeJSON(writer, http.StatusServiceUnavailable, errorView{
		Error: "this deployment has no sealing key and cannot hold a credential"})
	return false
}

// sealCredential closes over a pasted credential, bound to the row it will live on, and
// mints its identity — answering the operator when either fails. Both paths that accept
// a credential refuse through here, so they cannot drift into refusing differently.
func (h Handlers) sealCredential(
	writer http.ResponseWriter, credential string, id uuid.UUID,
) ([]byte, string, bool) {
	sealed, err := h.Sealer.Seal(credential, CredentialBinding(id))
	if err == nil {
		if fingerprint, mintErr := MintFingerprint(); mintErr == nil {
			return sealed, fingerprint, true
		}
	}
	writeJSON(writer, http.StatusServiceUnavailable,
		errorView{Error: "the credential could not be stored; nothing was saved"})
	return nil, "", false
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

func (h Handlers) enable(writer http.ResponseWriter, request *http.Request) {
	h.setDisabled(writer, request, false)
}

func (h Handlers) disable(writer http.ResponseWriter, request *http.Request) {
	h.setDisabled(writer, request, true)
}

func (h Handlers) setDisabled(
	writer http.ResponseWriter, request *http.Request, disabled bool,
) {
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

	if err := h.Store.SetIntegrationDisabled(
		ctx, principal, organization, id, disabled); err != nil {
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

	// An outbound type is verified by asking the provider itself; nothing gathered here
	// could say whether the credential still works.
	if definition.Probe != nil {
		verified, probeErr := h.Store.RecordIntegrationVerification(
			ctx, principal, organization, id, h.probeExisting(ctx, organization, definition, found))
		if probeErr != nil {
			h.fail(writer, request, probeErr)
			return
		}
		writeJSON(writer, http.StatusOK, h.viewOf(verified))
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

// probeExisting asks the provider about an Integration as recorded, unsealing its
// credential for the one call that presents it. A credential that cannot be opened —
// a rotated key, a tampered column, a blob moved from another row — is judged failed
// with what the operator can do about it, because "this integration cannot
// authenticate" is its operational truth. The unseal lands in the audit record first;
// one that cannot be recorded is not used.
func (h Handlers) probeExisting(
	ctx context.Context, organization tenancy.Organization, definition Definition,
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

// checkConfiguration reads submitted configuration against the type's declared fields.
// An undeclared field is refused rather than dropped, and a declared secret field is
// EXTRACTED rather than kept: the returned configuration never holds a credential, and
// the credential is returned beside it for the sealer. requireSecret distinguishes
// creation, where a required credential must arrive, from revision, where its absence
// means "keep the one I already gave you".
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
		if field.Recorded {
			// Written by the installation flow and never by a caller. Refused rather than
			// ignored: a value silently dropped is a caller who believes they set it.
			return nil, "", "configuration field " + strconv.Quote(name) +
				" is recorded by the connect flow and cannot be set"
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
				return nil, "", "configuration field " + strconv.Quote(field.Name) + " is required"
			}
			continue
		}
		if _, present := checked[field.Name]; !present {
			return nil, "", "configuration field " + strconv.Quote(field.Name) + " is required"
		}
	}
	return checked, credential, ""
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
	}
	return ""
}

// listQuery reads what a listing may be narrowed by, refusing anything the listing does
// not declare: a filter silently dropped returns everything while looking narrowed.
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
