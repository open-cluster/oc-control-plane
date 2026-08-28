package identity

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/session"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

const noWayIn = "no way in is configured here"

func newFlowID() uuid.UUID              { return uuid.New() }
func nowPlus(d time.Duration) time.Time { return time.Now().Add(d) }

func (h Handlers) issueSession(writer http.ResponseWriter, request *http.Request,
	organization tenancy.Organization, user storage.User, memberships []authz.Membership,
	_ admission,
) error {
	token, digest, issued, detail, err := h.prepareSession(
		request, organization, user.ID, len(memberships))
	if err != nil {
		return err
	}
	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()
	if err = h.Database.IssueSession(ctx, organization, issued, digest, audit.Actor{
		Kind: audit.ActorUser, ID: user.ID.String(), DisplayName: displayNameOf(user)}, detail); err != nil {
		return err
	}
	session.Set(writer, token, issued.ExpiresAt)
	return nil
}

func (h Handlers) prepareSession(
	request *http.Request, organization tenancy.Organization, userID uuid.UUID, membershipCount int,
) (session.Token, []byte, session.Session, audit.Detail, error) {
	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()
	configured, _, err := h.Database.SessionPolicy(ctx, organization)
	if err != nil {
		return "", nil, session.Session{}, nil, err
	}
	lifetime := session.ClampLifetime(configured)
	token, digest, err := session.NewToken()
	if err != nil {
		return "", nil, session.Session{}, nil, err
	}
	now := time.Now().UTC()
	issued := session.Session{ID: uuid.New(), UserID: userID, Organization: organization.String(),
		IssuedAt: now, ExpiresAt: now.Add(lifetime), UserAgent: request.UserAgent(), Address: request.RemoteAddr}
	detail := audit.Detail{"expiresAt": issued.ExpiresAt.Format(time.RFC3339),
		"memberships": membershipCount, "requestId": authz.RequestIDFrom(request.Context())}
	return token, digest, issued, detail, nil
}

func (h Handlers) redirectURI() string {
	return strings.TrimSuffix(h.PublicURL, "/") + Base + "/auth/oidc/callback"
}

func (h Handlers) returnTarget(writer http.ResponseWriter, asked string) (string, bool) {
	if asked == "" {
		return "/", true
	}
	if !strings.HasPrefix(asked, "/") || strings.HasPrefix(asked, "//") ||
		strings.Contains(asked, "\\") || len(asked) > 512 {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "returnTo must be a path on this site"})
		return "", false
	}
	parsed, err := url.Parse(asked)
	if err != nil || parsed.IsAbs() || parsed.Host != "" {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "returnTo must be a path on this site"})
		return "", false
	}
	return asked, true
}

func (h Handlers) consoleTarget(returnTo string) string {
	if returnTo == "" {
		returnTo = "/"
	}
	return strings.TrimSuffix(h.ConsoleURL, "/") + returnTo
}
