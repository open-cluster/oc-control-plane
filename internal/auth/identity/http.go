package identity

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/api/listing"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/session"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

// maxRequestBody bounds what a caller may send. Every body on this surface is a handful of
// short fields, and an unbounded one is an allocation somebody else chooses.
const maxRequestBody = 64 * 1024

type errorView struct {
	Error string `json:"error"`
}

func writeJSON(writer http.ResponseWriter, status int, body any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(body)
}

func listQuery(writer http.ResponseWriter, request *http.Request, spec listing.Spec) (listing.Query, bool) {
	query, err := listing.Parse(request.URL.Query(), spec)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: err.Error()})
		return listing.Query{}, false
	}
	return query, true
}

func decode(writer http.ResponseWriter, request *http.Request, into any) bool {
	decoder := json.NewDecoder(io.LimitReader(request.Body, maxRequestBody))
	// Unknown fields are refused rather than ignored. A caller who misspelled a field name
	// would otherwise get a success whose effect is not what they asked for, and on this
	// surface the fields being misspelled decide who may sign in.
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "the body is not valid JSON"})
		return false
	}
	return true
}

func contextWithTimeout(
	request *http.Request, budget time.Duration,
) (context.Context, context.CancelFunc) {
	return context.WithTimeout(request.Context(), budget)
}

func (h Handlers) caller(request *http.Request) authz.Principal {
	return authz.MustPrincipal(request.Context())
}

func (h Handlers) organization(request *http.Request) uuid.UUID {
	return authz.MustPrincipal(request.Context()).Organization()
}

// identifier reads a UUID path segment, naming the segment in the refusal so an operator knows
// which one they got wrong.
func identifier(
	writer http.ResponseWriter, request *http.Request, segment string,
) (uuid.UUID, bool) {
	id, err := uuid.Parse(request.PathValue(segment))
	if err != nil {
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: segment + " is not an identity"})
		return uuid.Nil, false
	}
	return id, true
}

// fail answers an error, naming the ones a caller can act on.
//
// ErrNotAMember answers 404 and never 403. It is the same answer the guard gives, and it must
// stay the same answer: a 403 here would confirm the tenant exists to a caller the guard has
// already decided must not learn that.
func (h Handlers) fail(writer http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, storage.ErrNotAMember), errors.Is(err, storage.ErrUnknownOrganization):
		writeJSON(writer, http.StatusNotFound, errorView{Error: "organization not found"})
	case errors.Is(err, storage.ErrUserUnknown):
		writeJSON(writer, http.StatusNotFound, errorView{Error: "user not found"})
	case errors.Is(err, storage.ErrLocalCredentialUnknown):
		writeJSON(writer, http.StatusNotFound, errorView{Error: "local account not found"})
	case errors.Is(err, storage.ErrLocalAccountExists):
		writeJSON(writer, http.StatusConflict, errorView{Error: "local account already exists"})
	case errors.Is(err, storage.ErrMembershipUnknown):
		writeJSON(writer, http.StatusNotFound, errorView{Error: "membership not found"})
	case errors.Is(err, session.ErrUnknown):
		writeJSON(writer, http.StatusNotFound, errorView{Error: "session not found"})
	case errors.Is(err, storage.ErrLastAdmin):
		writeJSON(writer, http.StatusConflict, errorView{
			Error: "an organization must keep at least one admin; appoint another first"})
	case errors.Is(err, ErrProviderUnreachable):
		writeJSON(writer, http.StatusBadGateway,
			errorView{Error: "the identity provider could not be reached"})
	case errors.Is(err, storage.ErrBadCursor):
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: "cursor is not a page position from a previous response"})
	case errors.Is(err, storage.ErrAuditFailed):
		// The operation was rolled back because it could not be recorded. Saying so is the
		// point: an operator who was told "it worked" about a change with no audit row would
		// have a change nobody can attribute, which is the failure this design refuses.
		h.Logger.ErrorContext(request.Context(), "an operation was rolled back unrecorded",
			slog.String("path", request.URL.Path),
			slog.String("error", err.Error()))
		writeJSON(writer, http.StatusServiceUnavailable, errorView{
			Error: "the change was refused because it could not be recorded"})
	default:
		h.Logger.ErrorContext(request.Context(), "identity request failed",
			slog.String("path", request.URL.Path),
			slog.String("error", err.Error()))
		writeJSON(writer, http.StatusInternalServerError, errorView{Error: "request failed"})
	}
}

const Base = "/api/v1"

func (h Handlers) Routes() []authz.Route {
	return []authz.Route{
		{Method: http.MethodPut, Pattern: Base + "/auth/local/password", Handler: http.HandlerFunc(h.changeLocalPassword)},

		{Method: http.MethodGet, Pattern: Base + "/session", Handler: http.HandlerFunc(h.session)},
		{Method: http.MethodGet, Pattern: Base + "/permissions", Handler: http.HandlerFunc(h.permissions)},

		{Method: http.MethodGet, Pattern: Base + "/members", Permission: authz.MemberRead, Handler: http.HandlerFunc(h.listMembers)},
		{Method: http.MethodPost, Pattern: Base + "/local-users", Permission: authz.MemberManage, Handler: http.HandlerFunc(h.createMember)},
		{Method: http.MethodPatch, Pattern: Base + "/members/{user}", Permission: authz.MemberManage, Handler: http.HandlerFunc(h.setMember)},
		{Method: http.MethodDelete, Pattern: Base + "/members/{user}", Permission: authz.MemberManage, Handler: http.HandlerFunc(h.removeMember)},

		{Method: http.MethodGet, Pattern: Base + "/sessions", Handler: http.HandlerFunc(h.listSessions)},
		{Method: http.MethodDelete, Pattern: Base + "/sessions/{session}", Handler: http.HandlerFunc(h.revokeSession)},

		{Method: http.MethodGet, Pattern: Base + "/policy", Permission: authz.IdentityRead, Handler: http.HandlerFunc(h.readPolicy)},
		{Method: http.MethodPut, Pattern: Base + "/policy", Permission: authz.IdentityConfigure, Handler: http.HandlerFunc(h.writePolicy)},

		{Method: http.MethodGet, Pattern: Base + "/audit-events", Permission: authz.AuditRead, Handler: http.HandlerFunc(h.auditEvents)},
	}
}

type Authentication struct {
	LocalBootstrap http.Handler
	LocalSignIn    http.Handler
	OIDCStart      http.Handler
	OIDCCallback   http.Handler
	SignOut        http.Handler
}

func (h Handlers) Authentication(origin string) Authentication {
	return Authentication{
		LocalBootstrap: http.HandlerFunc(h.bootstrapLocalAdmin),
		LocalSignIn:    http.HandlerFunc(h.localSignIn),
		OIDCStart:      http.HandlerFunc(h.startDeploymentOIDCSignIn),
		OIDCCallback:   http.HandlerFunc(h.completeDeploymentOIDCSignIn),
		SignOut:        h.protectSignOut(origin),
	}
}

func (h Handlers) protectSignOut(origin string) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !authz.CookieOriginAllowed(request, origin) {
			writeJSON(writer, http.StatusForbidden, errorView{Error: "request origin is not allowed"})
			return
		}
		session.Clear(writer)
		principal, err := h.Resolve(request)
		if errors.Is(err, authz.ErrNoCredential) || errors.Is(err, authz.ErrCredentialRejected) {
			h.signOut(writer, request, authz.Principal{})
			return
		}
		if err != nil || principal.IsZero() {
			writeJSON(writer, http.StatusServiceUnavailable, errorView{Error: "authentication unavailable"})
			return
		}
		h.signOut(writer, request, principal)
	})
}
