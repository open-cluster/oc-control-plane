package identity

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/session"
	"github.com/open-cluster/oc-control-plane/internal/storage"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// noWayIn is the answer to every unauthenticated request that does not resolve to a live
// provider: an organization that does not exist, one that exists and has configured nothing,
// one whose provider was deleted, one whose provider is disabled.
//
// They are the same answer because the alternative is a tenant directory. An unauthenticated
// caller who could tell "no such organization" from "that organization has not configured
// sign-in" could enumerate every customer this deployment serves, and the sign-in route is the
// one route they can always reach.
const noWayIn = "no way in is configured here"

// signInProviders lets a console render a chooser for a tenant that configured more than one
// provider. It returns the identifier and the name and nothing else — not the issuer, which
// would name the customer's identity vendor to anyone who asked.
func (h Handlers) signInProviders(writer http.ResponseWriter, request *http.Request) {
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	providers, err := h.Placements.SignInProviders(ctx, organization)
	if err != nil {
		if errors.Is(err, storage.ErrUnknownOrganization) {
			writeJSON(writer, http.StatusNotFound, errorView{Error: noWayIn})
			return
		}
		h.fail(writer, request, err)
		return
	}

	choices := make([]providerChoiceView, 0, len(providers))
	for _, provider := range providers {
		if provider.Disabled() {
			continue
		}
		choices = append(choices, providerChoiceView{
			ID:   provider.ID.String(),
			Name: provider.Name,
		})
	}
	if len(choices) == 0 {
		writeJSON(writer, http.StatusNotFound, errorView{Error: noWayIn})
		return
	}
	writeJSON(writer, http.StatusOK, providerChoicesView{Providers: choices})
}

// startSignIn sends the browser to the tenant's identity provider.
//
// Everything that makes the return trip checkable is written here and never leaves this
// process: the PKCE verifier, the nonce, and the digest of the state. Only the state itself
// travels through the browser, which is why only its digest is stored.
func (h Handlers) startSignIn(writer http.ResponseWriter, request *http.Request) {
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	providerID, ok := identifier(writer, request, "provider")
	if !ok {
		return
	}
	returnTo, ok := h.returnTarget(writer, request.URL.Query().Get("returnTo"))
	if !ok {
		return
	}

	ctx, cancel := contextWithTimeout(request, signInTimeout)
	defer cancel()

	provider, err := h.Placements.IdentityProviderForSignIn(ctx, organization, providerID)
	if err != nil || provider.Disabled() {
		// One answer for every way this can fail to name a live provider. See noWayIn.
		writeJSON(writer, http.StatusNotFound, errorView{Error: noWayIn})
		return
	}

	// Which protocol is the provider's, not the caller's. A route per protocol would let a
	// caller choose how they are authenticated, which is a choice that belongs to the tenant.
	switch provider.Protocol {
	case storage.ProtocolSAML:
		h.startSAMLSignIn(writer, request, organization, provider, returnTo)
		return
	case storage.ProtocolOIDC:
	default:
		writeJSON(writer, http.StatusNotFound, errorView{Error: noWayIn})
		return
	}

	authorization, err := h.OIDC.Authorize(
		ctx, provider.Issuer, provider.ClientID, h.redirectURI(), scopes)
	if err != nil {
		h.Logger.WarnContext(ctx, "a sign-in could not be started",
			slog.String("organization", organization.String()),
			slog.String("provider_id", providerID.String()),
			slog.String("error", err.Error()))
		h.fail(writer, request, err)
		return
	}

	if err := h.Placements.StartSignIn(ctx, organization, storage.SignInFlow{
		ID:           newFlowID(),
		Organization: organization.String(),
		ProviderID:   provider.ID,
		CodeVerifier: authorization.CodeVerifier,
		Nonce:        authorization.Nonce,
		ReturnTo:     returnTo,
		ExpiresAt:    nowPlus(flowLifetime),
	}, authorization.State); err != nil {
		h.fail(writer, request, err)
		return
	}
	h.recordSignInStarted(request, organization, provider)

	http.Redirect(writer, request, authorization.URL, http.StatusFound)
}

// recordSignInStarted puts an attempt on the record before the browser leaves, so a sign-in
// that never completes is still visible. A run of started-and-never-completed attempts against
// one tenant is what credential stuffing against the identity provider looks like from here.
func (h Handlers) recordSignInStarted(
	request *http.Request, organization tenancy.Organization, provider storage.IdentityProvider,
) {
	h.record(request, organization, audit.Event{
		Organization:  organization.String(),
		Actor:         audit.System("sign-in"),
		Action:        audit.ActionSignInStarted,
		Target:        audit.Target{Kind: audit.TargetIdentityProvider, ID: provider.ID.String()},
		Outcome:       audit.OutcomeAllowed,
		SourceAddress: request.RemoteAddr,
		Detail:        audit.Detail{"protocol": provider.Protocol.String()},
	})
}

// newFlowID and nowPlus exist so the two protocols mint a flow the same way. They are trivial,
// and that is the point: two call sites that differed by a line would be two lifetimes.
func newFlowID() uuid.UUID              { return uuid.New() }
func nowPlus(d time.Duration) time.Time { return time.Now().Add(d) }

// completeSignIn takes the browser back from the identity provider and issues a session.
//
// The order is the order the refusals matter in. The state is redeemed FIRST, and redeeming it
// is a conditional update, so a replayed callback is refused before this process spends a call
// on somebody else's token endpoint. Then the code is exchanged and the identity token is
// verified against the issuer's own keys, with the nonce this flow recorded. Then the tenant's
// policy decides whether this person is admitted at all. Only then is a session issued.
func (h Handlers) completeSignIn(writer http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	if failure := query.Get("error"); failure != "" {
		// The provider refused. Its reason is the customer's identity provider talking, so it
		// is logged and not echoed: it is attacker-influenced text on an unauthenticated route.
		h.Logger.WarnContext(request.Context(), "an identity provider refused a sign-in",
			slog.String("caller", request.RemoteAddr))
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: "the identity provider refused this sign-in"})
		return
	}
	state, code := query.Get("state"), query.Get("code")
	if state == "" || code == "" {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "this is not a sign-in"})
		return
	}

	ctx, cancel := contextWithTimeout(request, signInTimeout)
	defer cancel()

	// A state that was never issued, has expired, or has already been redeemed is one refusal.
	// The redemption is atomic, so two presentations of the same authorization code cannot
	// both pass here — which is the replay defence that does not depend on the provider having
	// one.
	flow, err := h.Placements.RedeemSignIn(ctx, state)
	if err != nil {
		h.Logger.WarnContext(ctx, "a sign-in callback presented an unusable state",
			slog.String("caller", request.RemoteAddr))
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "this sign-in cannot be completed"})
		return
	}
	organization, err := tenancy.NewOrganization(flow.Organization)
	if err != nil {
		h.fail(writer, request, err)
		return
	}

	provider, err := h.Placements.IdentityProviderForSignIn(ctx, organization, flow.ProviderID)
	if err != nil {
		h.refuseSignIn(writer, request, organization, flow.ProviderID, "the provider is gone")
		return
	}
	clientSecret, err := h.Sealer.Open(provider.ClientSecretSealed, provider.ID[:])
	if err != nil {
		h.fail(writer, request, err)
		return
	}

	asserted, err := h.OIDC.Exchange(ctx, provider.Issuer, provider.ClientID, clientSecret,
		h.redirectURI(), code, flow.CodeVerifier, flow.Nonce)
	if err != nil {
		// A PKCE verifier mismatch, a replayed code the provider caught, a state that belonged
		// to another flow, a token minted for another client, an expired token — all of them
		// arrive here, and all of them are one answer to the browser and a named reason in the
		// record.
		h.refuseSignIn(writer, request, organization, provider.ID, err.Error())
		return
	}

	h.admitAndIssue(writer, request, organization, provider, asserted,
		asserted.Issuer, asserted.Subject, flow.ReturnTo)
}

// admitAndIssue is everything after a provider has been believed, and it is ONE path for both
// protocols.
//
// That matters more than the code it saves. The tenant's provisioning policy, the verified
// domain check, the group mapping, the user resolution and the session issue are the things a
// security reviewer asks about, and an answer that began "for OIDC..." would have to be given
// twice and could drift. What differs between the protocols is how a provider is believed;
// everything after that is the tenant's policy and is the same.
func (h Handlers) admitAndIssue(
	writer http.ResponseWriter, request *http.Request, organization tenancy.Organization,
	provider storage.IdentityProvider, asserted claims, issuer, subject, returnTo string,
) {
	ctx, cancel := contextWithTimeout(request, signInTimeout)
	defer cancel()

	admitted, err := admit(provider, asserted)
	if err != nil {
		h.refuseSignIn(writer, request, organization, provider.ID, "the policy does not admit")
		return
	}

	user, memberships, err := h.Placements.ResolveUser(ctx, organization, storage.Identity{
		Issuer:        issuer,
		Subject:       subject,
		Email:         asserted.Email,
		EmailVerified: bool(asserted.EmailVerified),
		DisplayName:   asserted.displayName(),
	}, admitted.Role)
	if err != nil {
		if errors.Is(err, storage.ErrUserDisabled) {
			h.refuseSignIn(writer, request, organization, provider.ID, "the account is disabled")
			return
		}
		h.fail(writer, request, err)
		return
	}

	if err := h.issueSession(writer, request, organization, user, memberships, admitted); err != nil {
		h.fail(writer, request, err)
		return
	}
	http.Redirect(writer, request, h.consoleTarget(returnTo), http.StatusFound)
}

// issueSession mints the credential and records the sign-in.
func (h Handlers) issueSession(
	writer http.ResponseWriter, request *http.Request, organization tenancy.Organization,
	user storage.User, memberships []authz.Membership, admitted admission,
) error {
	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	configured, _, err := h.Placements.SessionPolicy(ctx, organization)
	if err != nil {
		return err
	}
	lifetime := session.ClampLifetime(configured)

	token, digest, err := session.NewToken()
	if err != nil {
		return err
	}
	issued := session.Session{
		ID:           uuid.New(),
		UserID:       user.ID,
		Organization: organization.String(),
		IssuedAt:     time.Now().UTC(),
		ExpiresAt:    time.Now().UTC().Add(lifetime),
		UserAgent:    request.UserAgent(),
		Address:      request.RemoteAddr,
	}
	// The detail names the person, not the credential. The token has already been minted and
	// will be written to the response; nothing about it reaches the record.
	detail := audit.Detail{
		"expiresAt":   issued.ExpiresAt.Format(time.RFC3339),
		"memberships": len(memberships),
		"requestId":   authz.RequestIDFrom(request.Context()),
	}
	if admitted.MappedFromGroup != "" {
		detail["mappedFromGroup"] = admitted.MappedFromGroup
		detail["mappedToRole"] = string(admitted.Role)
	}

	// The row and the event commit together. A session that exists with nothing saying who it
	// belongs to is the failure the same-transaction rule exists to prevent, and this is the
	// one path where the actor is established for the first time.
	if err := h.Placements.IssueSession(ctx, organization, issued, digest, audit.Actor{
		Kind:        audit.ActorUser,
		ID:          user.ID.String(),
		DisplayName: displayNameOf(user),
	}, detail); err != nil {
		return err
	}

	session.Set(writer, token, issued.ExpiresAt)
	return nil
}

// refuseSignIn answers a browser and records the refusal.
//
// The browser is told one thing and the record is told the reason. That asymmetry is
// deliberate: whoever is at the other end of a failing sign-in may be the operator or may be
// somebody trying states, and the difference between "the verifier did not match" and "the
// policy does not admit you" is exactly what the second one wants.
func (h Handlers) refuseSignIn(
	writer http.ResponseWriter, request *http.Request,
	organization tenancy.Organization, provider uuid.UUID, because string,
) {
	h.Logger.WarnContext(request.Context(), "a sign-in was refused",
		slog.String("organization", organization.String()),
		slog.String("provider_id", provider.String()),
		slog.String("reason", because),
		slog.String("caller", request.RemoteAddr))

	h.record(request, organization, audit.Event{
		Organization:  organization.String(),
		Actor:         audit.System("sign-in"),
		Action:        audit.ActionSignInRefused,
		Target:        audit.Target{Kind: audit.TargetIdentityProvider, ID: provider.String()},
		Outcome:       audit.OutcomeDenied,
		SourceAddress: request.RemoteAddr,
		Detail:        audit.Detail{"reason": because},
	})

	writeJSON(writer, http.StatusForbidden, errorView{Error: "this sign-in cannot be completed"})
}

// record writes one event best-effort.
//
// Best-effort here and transactional everywhere else, and the difference is what the event is
// attached to. A sign-in refusal has no state change to roll back, so failing the response
// because the record could not be written would turn an unreachable database into a surface
// that answers 500 to callers it was correctly refusing anyway.
func (h Handlers) record(
	request *http.Request, organization tenancy.Organization, event audit.Event,
) {
	event.RequestID = authz.RequestIDFrom(request.Context())
	if err := h.Placements.RecordEvent(request.Context(), organization, event); err != nil {
		h.Logger.ErrorContext(request.Context(), "an audit event could not be written",
			slog.String("action", string(event.Action)),
			slog.String("error", err.Error()))
	}
}

// redirectURI is what the identity provider was registered with. It is built from configuration
// and never from the request's own Host header: a caller-controlled host in a redirect URI is
// how an authorization code is delivered somewhere else.
func (h Handlers) redirectURI() string {
	return strings.TrimSuffix(h.PublicURL, "/") + Base + "/sign-in/callback"
}

// returnTarget validates where the browser asked to be sent after signing in.
//
// Only a same-site absolute path is accepted. A value that reached a Location header
// unvalidated is an open redirect carrying this product's own domain, which is the shape a
// convincing phishing link takes. "//evil.example.com" is refused explicitly: it is a
// protocol-relative URL that reads as a path.
func (h Handlers) returnTarget(writer http.ResponseWriter, asked string) (string, bool) {
	if asked == "" {
		return "/", true
	}
	if !strings.HasPrefix(asked, "/") || strings.HasPrefix(asked, "//") ||
		strings.Contains(asked, "\\") || len(asked) > 512 {
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: "returnTo must be a path on this site"})
		return "", false
	}
	parsed, err := url.Parse(asked)
	if err != nil || parsed.IsAbs() || parsed.Host != "" {
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: "returnTo must be a path on this site"})
		return "", false
	}
	return asked, true
}

// consoleTarget is where the browser lands. The console's origin is configuration; the path is
// the one the sign-in was started with, already validated as a same-site path.
func (h Handlers) consoleTarget(returnTo string) string {
	if returnTo == "" {
		returnTo = "/"
	}
	return strings.TrimSuffix(h.ConsoleURL, "/") + returnTo
}
