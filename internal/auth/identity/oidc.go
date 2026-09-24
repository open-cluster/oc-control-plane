package identity

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	coreoidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

const (
	discoveryTimeout = 10 * time.Second
	exchangeTimeout  = 15 * time.Second
	maxProviderBody  = 512 * 1024
)

var ErrProviderUnreachable = errors.New("the identity provider could not be reached")

type OIDC struct{ Client *http.Client }

func NewOIDC() *OIDC {
	return &OIDC{Client: &http.Client{Timeout: exchangeTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

type AuthorizationRequest struct{ URL, State, CodeVerifier, Nonce string }

func (o *OIDC) Authorize(ctx context.Context, issuer, clientID, redirectURI string, scopes []string) (AuthorizationRequest, error) {
	provider, err := o.provider(ctx, issuer)
	if err != nil {
		return AuthorizationRequest{}, err
	}
	state, err := randomToken()
	if err != nil {
		return AuthorizationRequest{}, err
	}
	nonce, err := randomToken()
	if err != nil {
		return AuthorizationRequest{}, err
	}
	verifier := oauth2.GenerateVerifier()
	configuration := oauth2.Config{ClientID: clientID, Endpoint: provider.Endpoint(), RedirectURL: redirectURI, Scopes: scopes}
	return AuthorizationRequest{URL: configuration.AuthCodeURL(state, coreoidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), State: state, CodeVerifier: verifier, Nonce: nonce}, nil
}

func (o *OIDC) Exchange(ctx context.Context, issuer, clientID, clientSecret, redirectURI, code, verifier, nonce string) (claims, error) {
	provider, err := o.provider(ctx, issuer)
	if err != nil {
		return claims{}, err
	}
	exchangeCtx, cancel := context.WithTimeout(o.clientContext(ctx), exchangeTimeout)
	defer cancel()
	configuration := oauth2.Config{ClientID: clientID, ClientSecret: clientSecret, Endpoint: provider.Endpoint(), RedirectURL: redirectURI, Scopes: scopes}
	token, err := configuration.Exchange(exchangeCtx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return claims{}, fmt.Errorf("%w: the provider refused the authorization code", ErrTokenRefused)
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok || raw == "" {
		return claims{}, fmt.Errorf("%w: the token response contains no identity token", ErrTokenRefused)
	}
	verified, err := provider.Verifier(&coreoidc.Config{ClientID: clientID}).Verify(exchangeCtx, raw)
	if err != nil {
		return claims{}, fmt.Errorf("%w: verification failed", ErrTokenRefused)
	}
	var asserted claims
	if err := verified.Claims(&asserted); err != nil {
		return claims{}, fmt.Errorf("%w: claims are invalid", ErrTokenRefused)
	}
	if asserted.Nonce != nonce || strings.TrimSpace(asserted.Subject) == "" {
		return claims{}, fmt.Errorf("%w: token does not answer this sign-in", ErrTokenRefused)
	}
	return asserted, nil
}

func (o *OIDC) provider(ctx context.Context, issuer string) (*coreoidc.Provider, error) {
	if err := usableIssuer(issuer); err != nil {
		return nil, err
	}
	discoveryCtx, cancel := context.WithTimeout(o.clientContext(ctx), discoveryTimeout)
	defer cancel()
	provider, err := coreoidc.NewProvider(discoveryCtx, strings.TrimSuffix(issuer, "/"))
	if err != nil {
		return nil, fmt.Errorf("%w: discovery failed", ErrProviderUnreachable)
	}
	return provider, nil
}

func (o *OIDC) clientContext(ctx context.Context) context.Context {
	client := o.Client
	if client == nil {
		client = http.DefaultClient
	}
	copy := *client
	transport := copy.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	copy.Transport = responseLimitTransport{next: transport}
	return context.WithValue(ctx, oauth2.HTTPClient, &copy)
}

type responseLimitTransport struct{ next http.RoundTripper }

func (t responseLimitTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.next.RoundTrip(request)
	if err == nil && response.Body != nil {
		response.Body = struct {
			io.Reader
			io.Closer
		}{Reader: io.LimitReader(response.Body, maxProviderBody), Closer: response.Body}
	}
	return response, err
}

func usableIssuer(issuer string) error {
	parsed, err := url.Parse(issuer)
	if err != nil || parsed.Host == "" {
		return fmt.Errorf("%w: %q is not a URL", ErrProviderUnreachable, issuer)
	}
	if parsed.Scheme == "https" || parsed.Scheme == "http" && isLoopback(parsed.Hostname()) {
		return nil
	}
	return fmt.Errorf("%w: %q is not https", ErrProviderUnreachable, issuer)
}
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}
func randomToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("identity: minting a random value: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

var ErrTokenRefused = errors.New("the identity token was refused")

type claims struct {
	Issuer        string  `json:"iss"`
	Subject       string  `json:"sub"`
	Nonce         string  `json:"nonce"`
	Email         string  `json:"email"`
	EmailVerified anyBool `json:"email_verified"`
	Name          string  `json:"name"`
	PreferredName string  `json:"preferred_username"`
}

type anyBool bool

func (b *anyBool) UnmarshalJSON(data []byte) error {
	var value bool
	if err := json.Unmarshal(data, &value); err == nil {
		*b = anyBool(value)
		return nil
	}
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	*b = anyBool(strings.EqualFold(text, "true"))
	return nil
}

func (c claims) displayName() string {
	for _, candidate := range []string{c.Name, c.PreferredName} {
		if trimmed := strings.TrimSpace(candidate); trimmed != "" {
			return trimmed
		}
	}
	if local, _, found := strings.Cut(c.Email, "@"); found && local != "" {
		return local
	}
	return c.Subject
}
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
