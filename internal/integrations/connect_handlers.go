package integrations

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
)

const CallbackPath = "/api/v1/integrations/connect/callback"
const connectTimeout = 30 * time.Second
const refusedConnect = "this connection cannot be completed"

type connectOutcome string

const (
	outcomeConnected         connectOutcome = "connected"
	outcomeRefused           connectOutcome = "refused"
	outcomeUnproven          connectOutcome = "unproven"
	outcomeUnverified        connectOutcome = "unverified"
	outcomeInstallationTaken connectOutcome = "installation-taken"
)

func (o connectOutcome) status() int {
	if o == outcomeConnected {
		return http.StatusOK
	}
	return http.StatusBadRequest
}

func (o connectOutcome) note() string {
	switch o {
	case outcomeConnected:
		return "connected"
	case outcomeUnproven:
		return "the provider would not confirm that the account you authorized with can " +
			"administer what it returned, so nothing was connected"
	case outcomeUnverified:
		return "the association was proven and the provider did not then answer, so " +
			"nothing was connected; start again"
	case outcomeInstallationTaken:
		return "that provider installation is already connected to OpenCluster, so nothing was " +
			"connected; disconnect it there before connecting it here"
	default:
		return refusedConnect
	}
}

type connectStartedView struct {
	AuthorizationURL string `json:"authorizationUrl"`
	ExpiresAt        string `json:"expiresAt"`
}

func (h Handlers) startConnect(writer http.ResponseWriter, request *http.Request) {
	principal := h.caller(request)
	organization := h.organization(request)
	definition, known := h.Catalog.Lookup(Provider(strings.TrimSpace(request.PathValue("type"))))
	if !known {
		writeJSON(writer, http.StatusNotFound,
			errorView{Error: "this build serves no integration type by that key"})
		return
	}
	if !definition.Connectable() {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: definition.Name +
			" has no installation flow on this deployment; configure it with its settings " +
			"form instead"})
		return
	}
	if definition.Connect.SealsCredential && !h.holdsCredentials(writer) {
		return
	}
	if h.PublicURL == "" {
		writeJSON(writer, http.StatusServiceUnavailable, errorView{Error: "this deployment " +
			"has not been told its own public URL, so it cannot receive a provider callback"})
		return
	}

	returnTo, ok := h.returnTarget(writer, request.URL.Query().Get("returnTo"))
	if !ok {
		return
	}

	state, err := GenerateSecret()
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	authorization, err := definition.Connect.Authorize(ctx, state, h.callbackURL())
	if err != nil {
		h.fail(writer, request, err)
		return
	}

	flow := ConnectFlow{
		Organization: organization.String(),
		Provider:     definition.Key,
		Principal:    principal.UserID().String(),
		ReturnTo:     returnTo,
		ExpiresAt:    time.Now().Add(connectFlowLifetime),
	}
	if err := h.Store.StartConnectFlow(ctx, organization, flow, state); err != nil {
		h.fail(writer, request, err)
		return
	}
	h.Logger.InfoContext(ctx, "integration connect started",
		slog.String("org_id", organization.String()),
		slog.String("type", string(definition.Key)))

	writeJSON(writer, http.StatusOK, connectStartedView{
		AuthorizationURL: authorization,
		ExpiresAt:        stamp(flow.ExpiresAt),
	})
}

func (h Handlers) completeConnect(writer http.ResponseWriter, request *http.Request) {
	principal := h.caller(request)
	state := request.URL.Query().Get("state")
	if state == "" {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: refusedConnect})
		return
	}

	ctx, cancel := context.WithTimeout(request.Context(), connectTimeout)
	defer cancel()

	flow, err := h.Store.RedeemConnectFlow(ctx, state)
	if err != nil {
		if !errors.Is(err, ErrConnectFlowUnknown) {
			h.fail(writer, request, err)
			return
		}
		h.refuseConnect(writer, request, "", "the state did not resolve to a live flow")
		return
	}
	if flow.Principal != principal.UserID().String() {
		h.refuseConnect(writer, request, flow.ReturnTo, "another principal started it")
		return
	}
	organization, err := uuid.Parse(strings.TrimSpace(flow.Organization))
	if err != nil || organization == uuid.Nil {
		if err == nil {
			err = errors.New("invalid organization identifier")
		}
		h.fail(writer, request, err)
		return
	}
	if principal.Organization() != organization || !principal.Can(authz.IntegrationCreate) {
		writeJSON(writer, http.StatusNotFound, errorView{Error: "organization not found"})
		return
	}
	definition, known := h.Catalog.Lookup(flow.Provider)
	if !known || !definition.Connectable() {
		h.fail(writer, request, fmt.Errorf(
			"connect flow names provider %q, which this build no longer connects",
			flow.Provider))
		return
	}

	bound, err := definition.Connect.Redeem(ctx, ConnectReturn{
		Query: request.URL.Query(), Callback: h.callbackURL(),
	})
	if err != nil {
		h.Logger.WarnContext(ctx, "an integration connect could not be proven",
			slog.String("org_id", organization.String()),
			slog.String("type", string(definition.Key)),
			slog.String("reason", err.Error()))
		h.landConnect(writer, request, flow.ReturnTo, definition.Key, outcomeUnproven, "")
		return
	}

	h.record(ctx, writer, request, principal, organization, definition, flow.ReturnTo, bound)
}

func (h Handlers) record(
	ctx context.Context, writer http.ResponseWriter, request *http.Request,
	principal authz.Principal, organization uuid.UUID, definition Definition,
	returnTo string, bound ConnectBinding,
) {
	var existing Integration
	var err error
	if bound.Installation != nil {
		existing, _, err = h.Store.IntegrationByInstallation(
			ctx, definition.Key, bound.Installation.Key)
		if err == nil && existing.OrgID != organization.String() {
			err = ErrInstallationTaken
		}
	} else {
		err = ErrUnknown
	}
	switch {
	case err == nil:
		h.reconnect(ctx, writer, request, principal, organization, definition, returnTo,
			existing, bound)
		return
	case errors.Is(err, ErrInstallationTaken):
		h.landConnect(writer, request, returnTo, definition.Key, outcomeInstallationTaken, "")
		return
	case !errors.Is(err, ErrUnknown):
		h.fail(writer, request, err)
		return
	}

	wanted := NewIntegration{
		ID:            uuid.New(),
		Provider:      definition.Key,
		Name:          bound.Name,
		Configuration: bound.Configuration,
		Installation:  bound.Installation,
	}
	verification := definition.Probe(ctx, ProbeInput{
		Integration: Integration{
			Provider:      wanted.Provider,
			Name:          wanted.Name,
			Configuration: wanted.Configuration,
			Installation:  wanted.Installation,
		},
		Credential: bound.Credential,
	})
	if verification.Status == StatusFailed {
		h.Logger.WarnContext(ctx, "a proven integration connect did not verify",
			slog.String("org_id", organization.String()),
			slog.String("type", string(definition.Key)),
			slog.String("note", verification.Note))
		h.landConnect(writer, request, returnTo, definition.Key, outcomeUnverified, "")
		return
	}
	wanted.Verification = &verification

	if bound.Credential != "" {
		sealed, ok := h.sealCredential(writer, bound.Credential, wanted.ID)
		if !ok {
			return
		}
		wanted.CredentialSealed = sealed
	}

	created, err := h.Store.CreateIntegration(ctx, principal, organization, wanted)
	if errors.Is(err, ErrInstallationTaken) {
		h.Logger.WarnContext(ctx, "a connect named a provider installation already owned elsewhere",
			slog.String("org_id", organization.String()),
			slog.String("type", string(definition.Key)))
		h.landConnect(writer, request, returnTo, definition.Key, outcomeInstallationTaken, "")
		return
	}
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	h.Logger.InfoContext(ctx, "integration connected",
		slog.String("org_id", organization.String()),
		slog.String("integration_id", created.ID.String()),
		slog.String("type", string(definition.Key)))

	h.landConnect(writer, request, returnTo, definition.Key, outcomeFor(created), created.ID.String())
}

func (h Handlers) reconnect(
	ctx context.Context, writer http.ResponseWriter, request *http.Request,
	principal authz.Principal, organization uuid.UUID, definition Definition,
	returnTo string, existing Integration, bound ConnectBinding,
) {
	if bound.Credential == "" {
		verified, err := h.Store.RecordIntegrationVerification(ctx, principal, organization,
			existing.ID, h.probeExisting(ctx, organization, definition, existing))
		if err != nil {
			h.fail(writer, request, err)
			return
		}
		h.landConnect(writer, request, returnTo, definition.Key, outcomeFor(verified), verified.ID.String())
		return
	}

	verification := definition.Probe(ctx, ProbeInput{
		Integration: existing, Credential: bound.Credential,
	})
	if verification.Status == StatusFailed {
		h.Logger.WarnContext(ctx, "a reconnected integration did not verify",
			slog.String("org_id", organization.String()),
			slog.String("integration_id", existing.ID.String()),
			slog.String("type", string(definition.Key)),
			slog.String("note", verification.Note))
		h.landConnect(writer, request, returnTo, definition.Key, outcomeUnverified, existing.ID.String())
		return
	}

	sealed, ok := h.sealCredential(writer, bound.Credential, existing.ID)
	if !ok {
		return
	}
	verified, err := h.Store.ReplaceIntegrationCredential(ctx, principal, organization,
		existing.ID, Revision{}, sealed, verification, bound.Installation)
	if errors.Is(err, ErrInstallationTaken) {
		h.Logger.WarnContext(ctx, "a reconnect named a provider installation already owned elsewhere",
			slog.String("org_id", organization.String()),
			slog.String("type", string(definition.Key)))
		h.landConnect(writer, request, returnTo, definition.Key, outcomeInstallationTaken, existing.ID.String())
		return
	}
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	h.Logger.InfoContext(ctx, "integration reconnected",
		slog.String("org_id", organization.String()),
		slog.String("integration_id", verified.ID.String()),
		slog.String("type", string(definition.Key)))
	h.landConnect(writer, request, returnTo, definition.Key, outcomeFor(verified), verified.ID.String())
}

func outcomeFor(integration Integration) connectOutcome {
	if integration.Status == StatusVerified {
		return outcomeConnected
	}
	return outcomeUnverified
}

func (h Handlers) refuseConnect(
	writer http.ResponseWriter, request *http.Request, returnTo, because string,
) {
	h.Logger.WarnContext(request.Context(), "an integration connect callback was refused",
		slog.String("reason", because))
	h.landConnect(writer, request, returnTo, "", outcomeRefused, "")
}

func (h Handlers) landConnect(
	writer http.ResponseWriter,
	request *http.Request,
	returnTo string,
	typeKey Provider,
	outcome connectOutcome,
	id string,
) {
	countConnect(request.Context(), string(typeKey), outcome)

	target, sendable := h.consoleTarget(returnTo, outcome, id)
	if !sendable {
		writeJSON(writer, outcome.status(), connectLandedView{
			Outcome: string(outcome), IntegrationID: id, Note: outcome.note(),
		})
		return
	}
	http.Redirect(writer, request, target, http.StatusFound)
}

func (h Handlers) consoleTarget(
	returnTo string, outcome connectOutcome, id string,
) (string, bool) {
	if h.PublicURL == "" {
		return "", false
	}
	if returnTo == "" {
		returnTo = "/"
	}
	target, err := url.Parse(strings.TrimSuffix(h.PublicURL, "/") + returnTo)
	if err != nil {
		return "", false
	}
	parameters := target.Query()
	parameters.Set("connect", string(outcome))
	if id != "" {
		parameters.Set("integration", id)
	}
	target.RawQuery = parameters.Encode()
	return target.String(), true
}

type connectLandedView struct {
	Outcome       string `json:"connect"`
	IntegrationID string `json:"integrationId,omitempty"`
	Note          string `json:"note,omitempty"`
}

func (h Handlers) callbackURL() string {
	// Configuration avoids sending an authorization code to a caller-controlled Host.
	return strings.TrimSuffix(h.PublicURL, "/") + CallbackPath
}

func (h Handlers) returnTarget(writer http.ResponseWriter, asked string) (string, bool) {
	if asked == "" {
		return "/", true
	}
	// Reject protocol-relative targets because they redirect to another host.
	parsed, err := url.Parse(asked)
	if !strings.HasPrefix(asked, "/") || strings.HasPrefix(asked, "//") ||
		strings.Contains(asked, "\\") || len(asked) > 512 ||
		err != nil || parsed.IsAbs() || parsed.Host != "" {
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: "returnTo must be a path on this site"})
		return "", false
	}
	return asked, true
}
