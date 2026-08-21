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

	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// CallbackPath is where a provider returns the browser. It is ONE path for every provider
// and it names no tenant, because a vendor registration holds a single redirect URI and
// because a tenant read out of a callback's URL is a tenant the caller chose.
const CallbackPath = "/operator/v1/integrations/connect/callback"

// connectTimeout bounds one leg of the flow. The callback leg spends two vendor calls —
// the code exchange and the association check — and then a live probe.
const connectTimeout = 30 * time.Second

// refusedConnect is the answer to every callback whose state does not resolve to a live
// flow this caller started: unknown, expired, already consumed, or somebody else's.
//
// They are one answer for the reason the sign-in refusal is one answer. A caller who could
// tell "never issued" from "already used" learns which half of a guess landed, and the
// state is the only thing standing between a captured callback and a bound installation.
const refusedConnect = "this connection cannot be completed"

// The closed vocabulary the console is handed back. It is this build's own words: nothing
// a vendor said reaches a URL, because everything a vendor says on this route is text an
// attacker may have chosen.
const (
	outcomeConnected = "connected"
	// outcomeRefused covers every state failure, so the redirect says no more than the
	// JSON answer would.
	outcomeRefused = "refused"
	// outcomeUnproven is the documented attack failing: the provider would not confirm
	// that whoever authorized this can reach the installation the callback named.
	outcomeUnproven = "unproven"
	// outcomeUnverified is an association that was proven and an installation that then
	// did not answer. The Integration is not created; the note says why.
	outcomeUnverified = "unverified"
)

// connectStartedView is where to send the browser.
type connectStartedView struct {
	// AuthorizationURL is the provider's own installation screen. The console navigates
	// to it; account selection, repository selection and permission consent all happen
	// there, where the permissions live.
	AuthorizationURL string `json:"authorizationUrl"`
	ExpiresAt        string `json:"expiresAt"`
}

// startConnect begins a provider installation flow.
//
// Everything that makes the return trip checkable is written here and never leaves this
// process: which organization, which principal, where the browser should land, and the
// digest of the state. Only the state itself travels through the browser.
func (h Handlers) startConnect(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	definition, known := h.Catalog.Lookup(strings.TrimSpace(request.PathValue("type")))
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
	if h.PublicURL == "" {
		// The redirect URI is registered with the vendor and must be absolute. Assembling
		// one from the request's own Host header would let a caller choose where an
		// authorization code is delivered.
		writeJSON(writer, http.StatusServiceUnavailable, errorView{Error: "this deployment " +
			"has not been told its own public URL, so it cannot receive a provider callback"})
		return
	}

	// returnTo travels in the query rather than in a body: it is the only thing this
	// operation takes, and a body would be one more shape to get wrong for no gain.
	returnTo, ok := h.returnTarget(writer, request.URL.Query().Get("returnTo"))
	if !ok {
		return
	}

	state, err := GenerateSecret()
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	authorization, err := definition.Connect.Authorize(state, h.callbackURL())
	if err != nil {
		h.fail(writer, request, err)
		return
	}

	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	flow := ConnectFlow{
		ID:           uuid.New(),
		Organization: organization.String(),
		Type:         definition.ID,
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
		slog.String("type", definition.Key))

	writeJSON(writer, http.StatusOK, connectStartedView{
		AuthorizationURL: authorization,
		ExpiresAt:        stamp(flow.ExpiresAt),
	})
}

// completeConnect takes the browser back from the provider and binds the installation.
//
// The order is the order the refusals matter in. The state is redeemed FIRST, and redeeming
// it is a conditional update, so a replayed callback is refused before this process spends a
// call on the vendor. The organization comes from the row that was redeemed; an organization
// named in the query is not read at all. Then the provider is asked to PROVE that whoever
// authorized this can actually reach the installation the callback named — the check the
// whole flow exists for. Only then is anything recorded.
func (h Handlers) completeConnect(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
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
		// A link somebody else started is not this caller's to finish, and it is the same
		// answer an unknown state gets.
		h.refuseConnect(writer, request, flow.ReturnTo, "another principal started it")
		return
	}
	organization, err := tenancy.NewOrganization(flow.Organization)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	if !principal.Can(organization, authz.IntegrationCreate) {
		// A membership or a role revoked between starting and returning. Answered as the
		// authorization middleware would answer it, so this route is not a way to learn
		// that a tenant exists.
		writeJSON(writer, http.StatusNotFound, errorView{Error: "organization not found"})
		return
	}
	definition, known := h.Catalog.ByID(flow.Type)
	if !known || !definition.Connectable() {
		h.fail(writer, request, fmt.Errorf(
			"connect flow %s names type %d, which this build no longer connects",
			flow.ID, flow.Type))
		return
	}

	enrolment, err := definition.Connect.Redeem(ctx, ConnectReturn{
		Query: request.URL.Query(), Callback: h.callbackURL(),
	})
	if err != nil {
		// The documented attack lands here: a callback naming an installation the
		// authorized user cannot reach proves nothing and binds nothing.
		h.Logger.WarnContext(ctx, "an integration connect could not be proven",
			slog.String("org_id", organization.String()),
			slog.String("type", definition.Key),
			slog.String("reason", err.Error()))
		h.landConnect(writer, request, flow.ReturnTo, outcomeUnproven, "", err.Error())
		return
	}

	h.bindEnrolment(ctx, writer, request, principal, organization, definition, flow, enrolment)
}

// bindEnrolment records what a proven return established: the same installation connected
// again is re-verified rather than duplicated, and a new one is probed live and born
// verified in the transaction that creates it.
func (h Handlers) bindEnrolment(
	ctx context.Context, writer http.ResponseWriter, request *http.Request,
	principal authz.Principal, organization tenancy.Organization, definition Definition,
	flow ConnectFlow, enrolment Enrolment,
) {
	existing, err := h.Store.IntegrationConfiguredAs(
		ctx, organization, definition.ID, enrolment.Configuration)
	switch {
	case err == nil:
		// The customer changed an installation this tenant already has, and the provider
		// sent them back here. Prove, then re-verify what exists.
		verified, recordErr := h.Store.RecordIntegrationVerification(ctx, principal,
			organization, existing.ID, h.probeExisting(ctx, organization, definition, existing))
		if recordErr != nil {
			h.fail(writer, request, recordErr)
			return
		}
		h.landConnect(writer, request, flow.ReturnTo, outcomeFor(verified),
			verified.ID.String(), verified.VerifyNote)
		return
	case !errors.Is(err, ErrUnknown):
		h.fail(writer, request, err)
		return
	}

	wanted := NewIntegration{
		ID:            uuid.New(),
		Type:          definition.ID,
		Name:          enrolment.Name,
		Configuration: enrolment.Configuration,
		CreatedBy:     principal.ID(),
	}
	verification := definition.Probe(ctx, ProbeInput{Integration: Integration{
		Type:          wanted.Type,
		Name:          wanted.Name,
		Configuration: wanted.Configuration,
	}})
	if verification.Status == StatusFailed {
		// Proven association, and the installation would not answer. Nothing is recorded:
		// an Integration born failed is one an operator has to clean up.
		h.landConnect(writer, request, flow.ReturnTo, outcomeUnverified, "", verification.Note)
		return
	}
	wanted.Verification = &verification

	created, err := h.Store.CreateIntegration(ctx, principal, organization, wanted)
	if errors.Is(err, ErrNameTaken) {
		// The account's own name is taken by an Integration configured differently — a
		// reinstall on the same account under a new id is the ordinary case. Disambiguated
		// once, by something stable, rather than retried in a loop.
		wanted.Name = disambiguate(enrolment.Name, wanted.ID)
		created, err = h.Store.CreateIntegration(ctx, principal, organization, wanted)
	}
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	h.Logger.InfoContext(ctx, "integration connected",
		slog.String("org_id", organization.String()),
		slog.String("integration_id", created.ID.String()),
		slog.String("type", definition.Key))

	h.landConnect(writer, request, flow.ReturnTo, outcomeFor(created),
		created.ID.String(), created.VerifyNote)
}

// disambiguate names the second Integration for one account: reinstalling on the same
// account suggests the same name under a different installation. The suffix is the row's
// own identity rather than a counter, which would need a read to know where it had got to,
// and it is stable for the record it names.
func disambiguate(name string, id uuid.UUID) string {
	disambiguated := name + " (" + id.String()[:8] + ")"
	if len(disambiguated) > maxNameLength {
		disambiguated = disambiguated[:maxNameLength]
	}
	return disambiguated
}

// outcomeFor reports how the console should read what landed.
func outcomeFor(integration Integration) string {
	if integration.Status == StatusActive {
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
	h.landConnect(writer, request, returnTo, outcomeRefused, "", refusedConnect)
}

// landConnect puts the browser back where it started.
//
// The browser is standing here, so it is sent on rather than shown a JSON body: the whole
// point of the flow is that the customer lands back in OpenCluster with the integration
// already connected. The outcome and the integration's identity travel as query parameters
// from a closed vocabulary this build owns; the note is rendered into the answer only when
// there is nowhere to send the browser.
func (h Handlers) landConnect(
	writer http.ResponseWriter, request *http.Request, returnTo, outcome, id, note string,
) {
	if h.ConsoleURL == "" {
		writeJSON(writer, statusFor(outcome), connectLandedView{
			Outcome: outcome, IntegrationID: id, Note: note,
		})
		return
	}
	if returnTo == "" {
		returnTo = "/"
	}
	target, err := url.Parse(strings.TrimSuffix(h.ConsoleURL, "/") + returnTo)
	if err != nil {
		writeJSON(writer, statusFor(outcome), connectLandedView{
			Outcome: outcome, IntegrationID: id, Note: note,
		})
		return
	}
	parameters := target.Query()
	parameters.Set("connect", outcome)
	if id != "" {
		parameters.Set("integration", id)
	}
	target.RawQuery = parameters.Encode()
	http.Redirect(writer, request, target.String(), http.StatusFound)
}

// connectLandedView is what a deployment with no console origin answers instead of a
// redirect. The note is this build's own words or the provider package's, never a vendor's.
type connectLandedView struct {
	Outcome       string `json:"connect"`
	IntegrationID string `json:"integrationId,omitempty"`
	Note          string `json:"note,omitempty"`
}

func statusFor(outcome string) int {
	if outcome == outcomeConnected {
		return http.StatusOK
	}
	return http.StatusBadRequest
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
