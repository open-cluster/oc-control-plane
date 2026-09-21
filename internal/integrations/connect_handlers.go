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
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
)

// CallbackPath is where a provider returns the browser. It is ONE path for every provider,
// and it names no tenant, because a vendor registration holds a single redirect URI and
// because a tenant read out of a callback's URL is a tenant the caller chose.
const CallbackPath = "/api/v1/integrations/connect/callback"
const connectTimeout = 30 * time.Second
const refusedConnect = "this connection cannot be completed"

// connectOutcome is how a finished flow reads to whoever started it: one word from a
// closed vocabulary this build owns.
type connectOutcome string

const (
	outcomeConnected connectOutcome = "connected"
	// outcomeRefused covers every state failure, so a caller cannot tell an unknown state
	// from an expired one, a replayed one, or somebody else's.
	outcomeRefused connectOutcome = "refused"
	// outcomeUnproven is the documented attack failing: the provider would not confirm
	// that whoever authorized this can reach what the callback named.
	outcomeUnproven connectOutcome = "unproven"
	// outcomeUnverified is an association that was proven and a far end that then did not
	// answer. Nothing is created, and verifying the record again is what tells an operator
	// more.
	outcomeUnverified connectOutcome = "unverified"
	// outcomeInstallationTaken is a provider installation another Integration in this
	// deployment already owns. It says nothing about WHERE the other one is: an
	// organization is not a fact a caller in a different one may learn.
	outcomeInstallationTaken connectOutcome = "installation-taken"
)

// status is the answer where there is no console to send the browser to.
func (o connectOutcome) status() int {
	if o == outcomeConnected {
		return http.StatusOK
	}
	return http.StatusBadRequest
}

// note is what a landing says, in this build's words.
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

// connectStartedView is where to send the browser.
type connectStartedView struct {
	// AuthorizationURL is the provider's own installation screen. The console navigates
	// to it; account selection, repository selection and permission consent all happen
	// there, where the permissions live.
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
		// Before the browser leaves. A deployment that cannot seal will not be able to
		// store what this flow comes back with, and the customer would learn that only
		// after granting permissions in somebody else's product — with a live credential
		// in this process that it can neither keep nor withdraw.
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
		Principal:    principal.ID(),
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

// completeConnect takes the browser back from the provider and binds the installation.
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
	if flow.Principal != principal.ID() {
		h.refuseConnect(writer, request, flow.ReturnTo, "another principal started it")
		return
	}
	organization, err := tenancy.NewOrganization(flow.Organization)
	if err != nil {
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

// record writes what a proven return established: the same installation connected
// again is re-verified rather than duplicated, and a new one is probed live and born
// verified in the transaction that creates it.
func (h Handlers) record(
	ctx context.Context, writer http.ResponseWriter, request *http.Request,
	principal authz.Principal, organization tenancy.Organization, definition Definition,
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
			// Answered by sealCredential. Nothing is created: an Integration recorded
			// without the credential it needs would read as connected and never work.
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

// reconnect is the customer connecting an installation this tenant already has. The
// provider sent them back here, the association is proven, and what exists is re-verified
// rather than duplicated.
func (h Handlers) reconnect(
	ctx context.Context, writer http.ResponseWriter, request *http.Request,
	principal authz.Principal, organization tenancy.Organization, definition Definition,
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

// outcomeFor reports how the console should read what landed.
func outcomeFor(integration Integration) connectOutcome {
	if integration.Status == StatusVerified {
		return outcomeConnected
	}
	return outcomeUnverified
}

// refuseConnect answers a callback whose state proved nothing. The browser is told the same
// thing whichever way it failed; the log says which, because the operator running the
// deployment is the one audience that benefits and the log is not somewhere a caller reads.
func (h Handlers) refuseConnect(
	writer http.ResponseWriter, request *http.Request, returnTo, because string,
) {
	h.Logger.WarnContext(request.Context(), "an integration connect callback was refused",
		slog.String("reason", because))
	h.landConnect(writer, request, returnTo, "", outcomeRefused, "")
}

// landConnect puts the browser back where it started.
//
// The browser is standing here, so it is sent on rather than shown a JSON body: the whole
// point of the flow is that the customer lands back in OpenCluster with the integration
// already connected. A deployment that has not said where its console is answers the same
// facts as JSON rather than guessing an origin.
// landConnect is where every return trip ends, which is why the counter lives here: one call
// site cannot forget to count, and nine could.
//
// typeKey is empty for a callback refused before its flow was resolved — at that point this
// deployment genuinely does not know which type the browser was trying to connect.
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

// consoleTarget is where the browser lands, and whether there is anywhere to send it. The
// console's origin is configuration; the path is the one the flow started with, already
// validated as a same-site path.
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

// connectLandedView is what a deployment with no console origin answers instead of a
// redirect. Every field is this build's own: the outcome comes from the closed vocabulary
// above and the note is what that vocabulary means, so nothing a provider said is rendered
// here.
type connectLandedView struct {
	Outcome       string `json:"connect"`
	IntegrationID string `json:"integrationId,omitempty"`
	Note          string `json:"note,omitempty"`
}

// callbackURL is the redirect URI registered with the provider. It is built from configured
// origin rather than from the request, because a URI assembled from a caller-controlled Host
// header is how an authorization code is delivered somewhere else.
func (h Handlers) callbackURL() string {
	return strings.TrimSuffix(h.PublicURL, "/") + CallbackPath
}

// returnTarget validates where the browser asked to be sent afterwards.
//
// Only a same-site absolute path is accepted. A value that reached a Location header
// unvalidated is an open redirect carrying this product's own domain, which is the shape a
// convincing phishing link takes. "//evil.example.com" is refused explicitly: it is a
// protocol-relative URL that reads as a path.
func (h Handlers) returnTarget(writer http.ResponseWriter, asked string) (string, bool) {
	if asked == "" {
		return "/", true
	}
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
