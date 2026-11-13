package identity

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/session"
	"github.com/open-cluster/oc-control-plane/internal/correlation"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func (h Handlers) issueSession(
	writer http.ResponseWriter,
	request *http.Request,
	organization uuid.UUID,
	user storage.User,
	localPasswordHash string,
) error {
	token, digest, err := session.NewToken()
	if err != nil {
		return err
	}
	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()
	actor := audit.Actor{Kind: audit.ActorUser, ID: user.ID.String(), DisplayName: displayNameOf(user)}
	var issued session.Session
	detail := audit.Detail{"requestId": correlation.From(request.Context())}
	if localPasswordHash != "" {
		issued, err = h.Database.IssueLocalSession(ctx, organization, user.ID, digest, actor,
			request.RemoteAddr, detail, localPasswordHash)
	} else {
		issued, err = h.Database.IssueSession(ctx, organization, user.ID, digest, actor,
			request.RemoteAddr, detail)
	}
	if err != nil {
		return err
	}
	session.Set(writer, token, issued.ExpiresAt)
	return nil
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

func (h Handlers) refreshSession(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	principal := h.caller(request)
	token, _ := session.FromRequest(request)
	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	expires, changed, err := h.Database.RefreshSession(ctx, principal, session.Digest(token))
	if err != nil {
		switch {
		case errors.Is(err, session.ErrExpired):
			writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "unauthorized", "reason": string(authz.ReasonSessionExpired)})
		case errors.Is(err, session.ErrUnknown):
			writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "unauthorized", "reason": string(authz.ReasonRejected)})
		default:
			writeJSON(writer, http.StatusServiceUnavailable, errorView{Error: "authentication unavailable"})
		}
		return
	}
	if changed {
		session.Set(writer, token, expires)
	}
	writeJSON(writer, http.StatusOK, refreshView{ExpiresAt: expires.UTC()})
}

func (h Handlers) signOut(writer http.ResponseWriter, request *http.Request, principal authz.Principal) {
	if principal.IsEmpty() {
		session.Clear(writer)
		writeJSON(writer, http.StatusOK, signOutView{SignedOut: true})
		return
	}
	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	if err := h.Database.DeleteCurrentSession(ctx, principal); err != nil && !errors.Is(err, session.ErrUnknown) {
		h.fail(writer, request, err)
		return
	}
	session.Clear(writer)
	writeJSON(writer, http.StatusOK, signOutView{SignedOut: true})
}
