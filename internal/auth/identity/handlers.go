package identity

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/session"
	"github.com/open-cluster/oc-control-plane/internal/correlation"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

const (
	readTimeout   = 15 * time.Second
	signInTimeout = 30 * time.Second
	flowLifetime  = 10 * time.Minute
)

var scopes = []string{"openid", "email", "profile"}

type Bootstrap struct{ Digest []byte }

func (b Bootstrap) Configured() bool { return len(b.Digest) > 0 }

type Handlers struct {
	Database         *storage.Database
	Logger           *slog.Logger
	OIDC             *OIDC
	OIDCIssuer       string
	OIDCClientID     string
	OIDCClientSecret string
	PublicURL        string
	Bootstrap        Bootstrap
	SessionLifetime  time.Duration
}

func (h Handlers) Resolve(request *http.Request) (authz.Principal, error) {
	requestID := correlation.From(request.Context())

	if token, present := session.FromRequest(request); present {
		principal, err := h.fromSession(request, token)
		if err != nil {
			return authz.Principal{}, err
		}
		return principal.WithRequest(request.RemoteAddr, requestID), nil
	}
	return authz.Principal{}, authz.ErrNoCredential
}

func (h Handlers) fromSession(
	request *http.Request, token session.Token,
) (authz.Principal, error) {
	// Resolve current Membership on every request so removal takes effect without waiting for
	// the browser session to expire.
	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	signedIn, err := h.Database.SessionByToken(ctx, session.Digest(token))
	if err != nil {
		switch {
		case errors.Is(err, session.ErrExpired):
			return authz.Principal{}, authz.Refusal{Because: authz.ReasonSessionExpired}
		case errors.Is(err, session.ErrUnknown):
			return authz.Principal{}, authz.ErrCredentialRejected
		default:
			return authz.Principal{}, authz.ErrAuthenticationUnavailable
		}
	}

	principal, err := authz.NewPrincipal(signedIn.User.ID, signedIn.Session.ID,
		displayNameOf(signedIn.User), signedIn.User.Email, signedIn.Membership)
	if err != nil {
		return authz.Principal{}, authz.ErrCredentialRejected
	}
	return principal, nil
}

func bearerToken(header string) (string, bool) {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	return token, token != ""
}

func displayNameOf(user storage.User) string {
	if name := strings.TrimSpace(user.DisplayName); name != "" {
		return name
	}
	if email := strings.TrimSpace(user.Email); email != "" {
		return email
	}
	return user.ID.String()
}
