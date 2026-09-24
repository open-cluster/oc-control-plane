package identity

import (
	"errors"
	"net/http"
	"strings"

	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func (h Handlers) startDeploymentOIDCSignIn(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSpace(h.OIDCIssuer) == "" {
		writeJSON(w, http.StatusNotFound, errorView{Error: noWayIn})
		return
	}
	returnTo, ok := h.returnTarget(w, r.URL.Query().Get("returnTo"))
	if !ok {
		return
	}
	ctx, cancel := contextWithTimeout(r, signInTimeout)
	defer cancel()
	authorization, err := h.OIDC.Authorize(ctx, h.OIDCIssuer, h.OIDCClientID, h.redirectURI(), scopes)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	err = h.Database.StartDeploymentSignIn(ctx, storage.DeploymentSignInFlow{
		CodeVerifier: authorization.CodeVerifier,
		Nonce:        authorization.Nonce,
		ReturnTo:     returnTo,
		ExpiresAt:    nowPlus(flowLifetime)}, authorization.State)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	http.Redirect(w, r, authorization.URL, http.StatusFound)
}

func (h Handlers) completeDeploymentOIDCSignIn(w http.ResponseWriter, r *http.Request) {
	state, code := r.URL.Query().Get("state"), r.URL.Query().Get("code")
	if state == "" || code == "" {
		writeJSON(w, http.StatusBadRequest, errorView{Error: "this is not a sign-in"})
		return
	}
	ctx, cancel := contextWithTimeout(r, signInTimeout)
	defer cancel()
	flow, err := h.Database.RedeemDeploymentSignIn(ctx, state)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorView{Error: "this sign-in cannot be completed"})
		return
	}
	asserted, err := h.OIDC.Exchange(
		ctx,
		h.OIDCIssuer,
		h.OIDCClientID,
		h.OIDCClientSecret,
		h.redirectURI(),
		code,
		flow.CodeVerifier,
		flow.Nonce)

	if err != nil {
		writeJSON(w, http.StatusForbidden, errorView{Error: "this sign-in cannot be completed"})
		return
	}
	user, membership, err := h.Database.OIDCIdentity(ctx, storage.Identity{
		Issuer:        asserted.Issuer,
		Subject:       asserted.Subject,
		Email:         asserted.Email,
		EmailVerified: bool(asserted.EmailVerified),
		DisplayName:   asserted.displayName()})
	if err != nil {
		if errors.Is(err, storage.ErrLocalCredentialUnknown) || errors.Is(err, storage.ErrUserDisabled) {
			writeJSON(w, http.StatusForbidden, errorView{Error: "this sign-in cannot be completed"})
			return
		}
		h.fail(w, r, err)
		return
	}
	organization := membership.Organization
	if err = h.issueSession(w, r, organization, user, ""); err != nil {
		h.fail(w, r, err)
		return
	}
	http.Redirect(w, r, h.consoleTarget(flow.ReturnTo), http.StatusFound)
}
