// Package environment owns the customer-named scopes that group Connections.
//
// An Environment is a relevance and correctness boundary — it exists so that a staging failure
// cannot be cited as evidence for a production incident — and it is deliberately NOT an
// execution-isolation one. One Relay may hold credentials for Connections in several
// Environments, so a customer who needs production and staging isolated deploys separate
// Relays. That distinction is stated here because describing it to a customer as isolation
// would be a promise this model does not make.
//
// The routes live on the operator surface, which already exists, is already off the public
// interface, and already reads across tenants behind its own credential. This package owns
// what an Environment is and what its endpoints do; that surface owns who may reach them.
package environment

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/storage"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// readTimeout bounds how long one request may take, so an operator query cannot outlive the
// attention of whoever made it.
const readTimeout = 15 * time.Second

// maxNameLength matches the column. Validated here so an over-long name is an answer rather
// than a constraint violation surfacing as a server error.
const maxNameLength = 128

// Handlers is this capability's dependencies.
type Handlers struct {
	Placements *storage.Placements
	Logger     *slog.Logger
}

// Mount registers the environment routes behind the operator surface's own authorization.
// This package does not know how a caller is authenticated and must not: one surface owns
// that decision, and a second copy of it is a second place for it to be wrong.
func (h Handlers) Mount(mux *http.ServeMux, authorized func(http.Handler) http.Handler) {
	mux.Handle("GET /operator/v1/organizations/{organization}/environments",
		authorized(http.HandlerFunc(h.list)))
	mux.Handle("POST /operator/v1/organizations/{organization}/environments",
		authorized(http.HandlerFunc(h.create)))
	mux.Handle("PATCH /operator/v1/organizations/{organization}/environments/{environment}",
		authorized(http.HandlerFunc(h.rename)))
	mux.Handle("DELETE /operator/v1/organizations/{organization}/environments/{environment}",
		authorized(http.HandlerFunc(h.delete)))
}

// list reports an organization's scopes.
//
// It ensures the Default exists first. An Organization arrives from an external identity
// provider rather than being created here, so there is no organization-creating transaction
// for the Default to join; the first read is where the promise that one always exists is
// actually kept. Doing it on the read rather than the write means an operator who has never
// touched this API still finds a scope to put a Connection in.
func (h Handlers) list(writer http.ResponseWriter, request *http.Request) {
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	if _, err := h.Placements.EnsureDefaultEnvironment(ctx, organization); err != nil {
		h.fail(writer, request, err)
		return
	}

	list, err := h.Placements.ListEnvironments(ctx, organization, storage.Page{
		Limit: pageSize(request),
		After: request.URL.Query().Get("after"),
	})
	if err != nil {
		h.fail(writer, request, err)
		return
	}

	// Every read of this surface crosses a tenant boundary, so every read is recorded.
	h.Logger.InfoContext(ctx, "operator read environments",
		slog.String("organization", organization.String()),
		slog.Int("environments", len(list.Environments)),
		slog.String("caller", callerOf(request)))

	views := make([]environmentView, 0, len(list.Environments))
	for _, environment := range list.Environments {
		views = append(views, viewOf(environment))
	}
	writeJSON(writer, http.StatusOK, listView{Environments: views, Next: list.Next})
}

func (h Handlers) create(writer http.ResponseWriter, request *http.Request) {
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	var body nameRequest
	if !decode(writer, request, &body) {
		return
	}
	name, ok := validName(writer, body.Name)
	if !ok {
		return
	}

	// The Default must exist before anything else does, so that an organization whose first
	// action is creating a second scope still ends up with one marked Default.
	if _, err := h.Placements.EnsureDefaultEnvironment(ctx, organization); err != nil {
		h.fail(writer, request, err)
		return
	}

	created, err := h.Placements.CreateEnvironment(ctx, organization, name)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	h.Logger.InfoContext(ctx, "operator created an environment",
		slog.String("organization", organization.String()),
		slog.String("environment_id", created.ID.String()),
		slog.String("caller", callerOf(request)))
	writeJSON(writer, http.StatusCreated, viewOf(created))
}

// rename changes what a scope is called. The identity survives, which is the property
// everything pointing at an Environment depends on.
func (h Handlers) rename(writer http.ResponseWriter, request *http.Request) {
	organization, id, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	var body nameRequest
	if !decode(writer, request, &body) {
		return
	}
	name, ok := validName(writer, body.Name)
	if !ok {
		return
	}

	renamed, err := h.Placements.RenameEnvironment(ctx, organization, id, name)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	h.Logger.InfoContext(ctx, "operator renamed an environment",
		slog.String("organization", organization.String()),
		slog.String("environment_id", renamed.ID.String()),
		slog.String("caller", callerOf(request)))
	writeJSON(writer, http.StatusOK, viewOf(renamed))
}

func (h Handlers) delete(writer http.ResponseWriter, request *http.Request) {
	organization, id, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	if err := h.Placements.DeleteEnvironment(ctx, organization, id); err != nil {
		h.fail(writer, request, err)
		return
	}
	h.Logger.InfoContext(ctx, "operator deleted an environment",
		slog.String("organization", organization.String()),
		slog.String("environment_id", id.String()),
		slog.String("caller", callerOf(request)))
	writer.WriteHeader(http.StatusNoContent)
}

// validName checks what an operator typed. A name is trimmed and then taken as written:
// silently rewriting it would mean the scope they see is not the one they named.
func validName(writer http.ResponseWriter, name string) (string, bool) {
	trimmed := strings.TrimSpace(name)
	switch {
	case trimmed == "":
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "name must not be empty"})
		return "", false
	case len(trimmed) > maxNameLength:
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: "name must be at most " + strconv.Itoa(maxNameLength) + " characters"})
		return "", false
	}
	for _, character := range trimmed {
		if unicode.IsControl(character) {
			writeJSON(writer, http.StatusBadRequest,
				errorView{Error: "name must not contain control characters"})
			return "", false
		}
	}
	return trimmed, true
}

// fail answers an error, naming the ones an operator can act on. The caller is acting on their
// own tenant, so saying which it was costs nothing and saves them guessing.
func (h Handlers) fail(writer http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, storage.ErrUnknownOrganization):
		writeJSON(writer, http.StatusNotFound, errorView{Error: "organization not served here"})
	case errors.Is(err, storage.ErrEnvironmentUnknown):
		writeJSON(writer, http.StatusNotFound, errorView{Error: "environment not found"})
	case errors.Is(err, storage.ErrEnvironmentNameTaken):
		writeJSON(writer, http.StatusConflict,
			errorView{Error: "another environment in this organization already has that name"})
	case errors.Is(err, storage.ErrEnvironmentIsDefault):
		writeJSON(writer, http.StatusConflict,
			errorView{Error: "the default environment cannot be deleted; it can be renamed"})
	case errors.Is(err, storage.ErrEnvironmentInUse):
		writeJSON(writer, http.StatusConflict,
			errorView{Error: "this environment still has connections; remove them first"})
	case errors.Is(err, storage.ErrBadCursor):
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: "after is not a page position from a previous response"})
	default:
		h.Logger.ErrorContext(request.Context(), "operator environment request failed",
			slog.String("path", request.URL.Path),
			slog.String("error", err.Error()))
		writeJSON(writer, http.StatusInternalServerError, errorView{Error: "request failed"})
	}
}

// organization resolves the tenant named in the path. Unlike intake, an operator route names
// its tenant: this surface reads across them by design, behind a credential of its own.
func (h Handlers) organization(
	writer http.ResponseWriter, request *http.Request,
) (tenancy.Organization, bool) {
	organization, err := tenancy.NewOrganization(request.PathValue("organization"))
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "organization is not a name"})
		return tenancy.Organization{}, false
	}
	return organization, true
}

func (h Handlers) addressed(
	writer http.ResponseWriter, request *http.Request,
) (tenancy.Organization, uuid.UUID, bool) {
	organization, ok := h.organization(writer, request)
	if !ok {
		return tenancy.Organization{}, uuid.UUID{}, false
	}
	id, err := uuid.Parse(request.PathValue("environment"))
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "environment is not an identity"})
		return tenancy.Organization{}, uuid.UUID{}, false
	}
	return organization, id, true
}

// callerOf reports where a request came from. It is the only identity this surface has: one
// shared token cannot say who, only from where.
func callerOf(request *http.Request) string { return request.RemoteAddr }

// pageSize reads how many were asked for. An unreadable value is not an error: the bound is
// the point, and storage clamps whatever arrives.
func pageSize(request *http.Request) int {
	size, err := strconv.Atoi(request.URL.Query().Get("limit"))
	if err != nil {
		return 0
	}
	return size
}
