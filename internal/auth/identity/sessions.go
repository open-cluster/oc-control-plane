package identity

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/api/listing"
	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/session"
	"github.com/open-cluster/oc-control-plane/internal/correlation"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

const noWayIn = "no way in is configured here"

func nowPlus(d time.Duration) time.Time {
	return time.Now().Add(d)
}

func (h Handlers) issueSession(
	writer http.ResponseWriter,
	request *http.Request,
	organization uuid.UUID,
	user storage.User,
	localPasswordHash string,
) error {
	token, digest, issued, detail, err := h.prepareSession(request, organization, user.ID)
	if err != nil {
		return err
	}
	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()
	actor := audit.Actor{Kind: audit.ActorUser, ID: user.ID.String(), DisplayName: displayNameOf(user)}
	if localPasswordHash != "" {
		_, err = h.Database.IssueLocalSession(ctx, organization, issued, digest, actor, detail, localPasswordHash)
	} else {
		_, err = h.Database.IssueSession(ctx, organization, issued, digest, actor, detail)
	}
	if err != nil {
		return err
	}
	session.Set(writer, token, issued.ExpiresAt)
	return nil
}

func (h Handlers) prepareSession(
	request *http.Request,
	organization uuid.UUID,
	userID uuid.UUID,
) (session.Token, []byte, session.Session, audit.Detail, error) {
	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()
	if organization != uuid.Nil {
		_, err := h.Database.OrganizationAuditRetention(ctx, organization)
		if err != nil {
			return "", nil, session.Session{}, nil, err
		}
	}
	token, digest, issued, err := session.Issue(userID, h.SessionLifetime)
	if err != nil {
		return "", nil, session.Session{}, nil, err
	}
	issued.ClientUserAgent = request.UserAgent()
	issued.RemoteAddr = request.RemoteAddr
	detail := audit.Detail{"expiresAt": issued.ExpiresAt.Format(time.RFC3339),
		"requestId": correlation.From(request.Context())}
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
	return strings.TrimSuffix(h.PublicURL, "/") + returnTo
}

func (h Handlers) session(writer http.ResponseWriter, request *http.Request) {
	principal := h.caller(request)

	writeJSON(writer, http.StatusOK, sessionViewOf(principal))
}

func (h Handlers) signOut(writer http.ResponseWriter, request *http.Request, principal authz.Principal) {
	if principal.IsZero() {
		writeJSON(writer, http.StatusOK, signOutView{SignedOut: true})
		return
	}
	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	if err := h.Database.RevokeCurrentSession(ctx, principal, principal.SessionID()); err != nil && !errors.Is(err, session.ErrUnknown) {
		h.fail(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, signOutView{SignedOut: true})
}

func (h Handlers) listSessions(writer http.ResponseWriter, request *http.Request) {
	query, ok := listQuery(writer, request, listing.Spec{
		DefaultSort: listing.Sort{Field: "lastSeenAt", Descending: true},
	})
	if !ok {
		return
	}
	principal := h.caller(request)
	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	live, err := h.Database.ListSessions(ctx, principal, storage.Page{
		Limit: query.Limit, After: query.Cursor,
	})
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	views := make([]liveSessionView, 0, len(live.Sessions))
	for _, found := range live.Sessions {
		views = append(views, liveSessionViewOf(found))
	}
	writeJSON(writer, http.StatusOK, liveSessionListView{
		Sessions: views, Next: listing.CursorPtr(live.Next),
	})
}

func (h Handlers) revokeSession(writer http.ResponseWriter, request *http.Request) {
	principal := h.caller(request)
	sessionID, ok := identifier(writer, request, "session")
	if !ok {
		return
	}
	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()
	if err := h.Database.RevokeSession(ctx, principal, sessionID); err != nil {
		h.fail(writer, request, err)
		return
	}
	if principal.SessionID() == sessionID {
		session.Clear(writer)
	}
	writer.WriteHeader(http.StatusNoContent)
}
