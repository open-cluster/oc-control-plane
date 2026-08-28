package identity

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/secrets"
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

// decode reads a JSON body, answering the caller itself on a refusal so a handler either has a
// body or has already returned.
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

// contextWithTimeout bounds one call. It is separate from the handler bodies so the bound is
// visible in one place rather than repeated with a different number each time.
func contextWithTimeout(
	request *http.Request, budget time.Duration,
) (context.Context, context.CancelFunc) {
	return context.WithTimeout(request.Context(), budget)
}

// caller resolves the principal the guard put on this request.
//
// A handler behind the guard always has one; the absence is a route mounted outside the table,
// which is a programming error rather than a runtime condition. It answers 500 with a log line
// rather than panicking, so the failure is one route rather than the process.
func (h Handlers) caller(
	writer http.ResponseWriter, request *http.Request,
) (authz.Principal, bool) {
	principal, ok := authz.Of(request)
	if !ok {
		h.Logger.ErrorContext(request.Context(),
			"a handler ran with no principal; the route is mounted outside the permission table",
			slog.String("path", request.URL.Path))
		writeJSON(writer, http.StatusInternalServerError, errorView{Error: "request failed"})
		return authz.Principal{}, false
	}
	return principal, true
}

// organization returns the tenant verified by the authorization middleware.
func (h Handlers) organization(
	writer http.ResponseWriter, request *http.Request,
) (tenancy.Organization, bool) {
	organization, ok := authz.ActiveOrganizationFrom(request.Context())
	if !ok {
		h.Logger.ErrorContext(request.Context(),
			"a handler ran with no verified active organization",
			slog.String("path", request.URL.Path))
		writeJSON(writer, http.StatusInternalServerError, errorView{Error: "request failed"})
		return tenancy.Organization{}, false
	}
	return organization, true
}

// preAuthenticationOrganization parses the compatibility path before a Principal exists.
func (h Handlers) preAuthenticationOrganization(
	writer http.ResponseWriter, request *http.Request,
) (tenancy.Organization, bool) {
	organization, err := tenancy.NewOrganization(request.PathValue("organization"))
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "organization is not a name"})
		return tenancy.Organization{}, false
	}
	return organization, true
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
	case errors.Is(err, storage.ErrLastAdmin):
		writeJSON(writer, http.StatusConflict, errorView{
			Error: "an organization must keep at least one admin; appoint another first"})
	case errors.Is(err, seal.ErrNoKey):
		writeJSON(writer, http.StatusServiceUnavailable, errorView{
			Error: "this deployment has no sealing key and cannot hold a client secret"})
	case errors.Is(err, ErrProviderUnreachable):
		writeJSON(writer, http.StatusBadGateway,
			errorView{Error: "the identity provider could not be reached"})
	case errors.Is(err, storage.ErrBadCursor):
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: "after is not a page position from a previous response"})
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
		h.Logger.ErrorContext(request.Context(), "operator identity request failed",
			slog.String("path", request.URL.Path),
			slog.String("error", err.Error()))
		writeJSON(writer, http.StatusInternalServerError, errorView{Error: "request failed"})
	}
}

// pageSize reads how many were asked for. An unreadable value is not an error: the bound is
// the point, and storage clamps whatever arrives.
func pageSize(request *http.Request) int {
	size, err := strconv.Atoi(request.URL.Query().Get("limit"))
	if err != nil {
		return 0
	}
	return size
}
