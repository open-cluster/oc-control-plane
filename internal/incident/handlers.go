package incident

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/table"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// The routes an operator reads and corrects a grouping through.
//
// They live on the operator surface, which already owns who may reach them. This package owns what
// they mean and what they return.

const (
	// readTimeout bounds one read, so a query cannot outlive the attention of whoever made it.
	readTimeout = 15 * time.Second
	// mergeTimeout is longer because a merge writes, and a write that times out halfway is worse
	// than one that takes a moment.
	mergeTimeout = 30 * time.Second
)

// Handlers is this capability's dependencies.
type Handlers struct {
	Store  Store
	Logger *slog.Logger
}

// Routes is this capability's contribution to the operator API's index.
//
// Reading a grouping and CHANGING one are separate permissions. Reading is what everybody looking
// at the tenant does; regrouping decides what an incident is about, and so what an investigation
// opened for it would be scoped to.
func (h Handlers) Routes() authz.Table {
	const base = "/operator/v1/organizations/{organization}/incidents"

	return authz.Table{
		authz.Privileged(http.MethodGet, base, authz.IncidentRead,
			http.HandlerFunc(h.list)),
		authz.Privileged(http.MethodGet, base+"/{episode}", authz.IncidentRead,
			http.HandlerFunc(h.episode)),
		authz.Privileged(http.MethodGet, base+"/{episode}/signals", authz.IncidentRead,
			http.HandlerFunc(h.signals)),
		authz.Privileged(http.MethodPost, base+"/{episode}/merge", authz.IncidentMerge,
			http.HandlerFunc(h.merge)),
	}
}

// episodesSpec is what the episode listing accepts.
//
// The grouping key is searchable because it is what an operator arrives holding: they were paged
// by their own alerting, which named the group, and being unable to find the episode by it would
// mean reading the list.
var episodesSpec = table.Spec{
	Searchable:  true,
	Sortable:    []string{"lastSeenAt", "firstSeenAt", "title", "signalCount"},
	DefaultSort: table.Sort{Field: "lastSeenAt", Descending: true},
	Filters:     []string{"environmentId", "connectionId", "status"},
}

func (h Handlers) list(writer http.ResponseWriter, request *http.Request) {
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	parsed, ok := h.query(writer, request, episodesSpec)
	if !ok {
		return
	}
	narrowed, ok := h.narrowing(writer, parsed)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	page, err := h.Store.QueryEpisodes(ctx, organization, narrowed)
	if err != nil {
		h.fail(writer, request, err)
		return
	}

	views := make([]episodeView, 0, len(page.Episodes))
	for _, found := range page.Episodes {
		views = append(views, viewOf(found))
	}
	// No total. Counting a cursor-paginated listing costs a second scan of everything the filters
	// matched, and a null count is worth more than a number somebody would trust.
	writeJSON(writer, http.StatusOK, table.Answer(views, page.Next, nil))
}

func (h Handlers) episode(writer http.ResponseWriter, request *http.Request) {
	organization, id, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	found, err := h.Store.Episode(ctx, organization, id)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, viewOf(found))
}

func (h Handlers) signals(writer http.ResponseWriter, request *http.Request) {
	organization, id, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	list, err := h.Store.EpisodeSignals(ctx, organization, id, pageOf(request))
	if err != nil {
		h.fail(writer, request, err)
		return
	}

	views := make([]signalView, 0, len(list.Signals))
	for _, found := range list.Signals {
		views = append(views, signalViewOf(found))
	}
	writeJSON(writer, http.StatusOK, table.Answer(views, list.Next, nil))
}

// merge records that two episodes an operator is looking at are one incident.
//
// The episode in the PATH is the one that gives way, and the body names the one that survives. That
// direction is the one an operator is in: they are looking at a duplicate and saying where it
// belongs, rather than looking at the survivor and listing what to absorb into it.
func (h Handlers) merge(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, id, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	var body mergeRequest
	if !decode(writer, request, &body) {
		return
	}
	into, err := uuid.Parse(body.Into)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: "into is not an episode identity"})
		return
	}

	wanted := Merge{Absorbed: id, Into: into, Reason: body.Reason}
	if err = wanted.Validate(); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(request.Context(), mergeTimeout)
	defer cancel()

	surviving, err := h.Store.MergeEpisodes(ctx, principal, organization, wanted)
	if err != nil {
		h.fail(writer, request, err)
		return
	}

	// Identifiers and counts. Never a title and never a Signal's text: both are what a customer's
	// systems produced and a log that quoted them would turn diagnosis into a disclosure channel.
	h.Logger.InfoContext(ctx, "incident episodes merged",
		slog.String("organization", organization.String()),
		slog.String("absorbed_episode_id", id.String()),
		slog.String("surviving_episode_id", into.String()),
		slog.String("caller", h.callerName(request)))

	writeJSON(writer, http.StatusOK, viewOf(surviving))
}

// narrowing turns the parsed query into what the store needs, refusing every filter value that
// could never match.
//
// A value nobody serves is REFUSED rather than narrowed to nothing. An empty page is exactly what
// "this tenant has no resolved incidents" looks like, so a caller who wrote `resolvd` would read a
// correct-looking answer to a question they did not ask.
func (h Handlers) narrowing(writer http.ResponseWriter, query table.Query) (Query, bool) {
	narrowed := Query{
		Search:     query.Search,
		Sort:       query.Sort.Field,
		Descending: query.Sort.Descending,
		Cursor:     query.Cursor,
		Limit:      query.Limit,
	}

	if named := query.Filter("environmentId"); named != "" {
		environment, err := uuid.Parse(named)
		if err != nil {
			writeJSON(writer, http.StatusBadRequest,
				errorView{Error: "environmentId is not an identity"})
			return Query{}, false
		}
		narrowed.Environment = &environment
	}
	if named := query.Filter("connectionId"); named != "" {
		connection, err := uuid.Parse(named)
		if err != nil {
			writeJSON(writer, http.StatusBadRequest,
				errorView{Error: "connectionId is not an identity"})
			return Query{}, false
		}
		narrowed.Connection = &connection
	}
	if named := query.Filter("status"); named != "" {
		status, known := ParseStatus(named)
		if !known {
			writeJSON(writer, http.StatusBadRequest,
				errorView{Error: "status is one of open or resolved"})
			return Query{}, false
		}
		narrowed.Status = status
	}
	return narrowed, true
}

func (h Handlers) query(
	writer http.ResponseWriter, request *http.Request, spec table.Spec,
) (table.Query, bool) {
	parsed, err := table.Parse(request.URL.Query(), spec)
	if err != nil {
		if table.Refused(err) {
			writeJSON(writer, http.StatusBadRequest, errorView{Error: err.Error()})
			return table.Query{}, false
		}
		// A Spec whose own default sort it does not offer is a programming error rather than a
		// caller's mistake, so it is logged as one and answered as one.
		h.Logger.ErrorContext(request.Context(), "a listing declares a query it cannot serve",
			slog.String("path", request.URL.Path),
			slog.String("error", err.Error()))
		writeJSON(writer, http.StatusInternalServerError, errorView{Error: "request failed"})
		return table.Query{}, false
	}
	return parsed, true
}

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
	id, err := uuid.Parse(request.PathValue("episode"))
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "episode is not an identity"})
		return tenancy.Organization{}, uuid.UUID{}, false
	}
	return organization, id, true
}

// caller resolves the principal the guard put on this request. A handler behind the guard always
// has one; its absence is a route mounted outside the permission table, which is a programming
// error answered with a 500 and a log line rather than a panic.
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

func (h Handlers) callerName(request *http.Request) string {
	principal, ok := authz.Of(request)
	if !ok {
		return request.RemoteAddr
	}
	return principal.DisplayName() + " (" + request.RemoteAddr + ")"
}

// fail answers an error, naming the ones a caller can act on.
//
// An episode this organization does not have and one that is another organization's are ONE
// answer, for the same reason the investigation surface gives: telling them apart would let a
// caller compose path parameters until one of them landed.
func (h Handlers) fail(writer http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, authz.ErrNotAMember):
		// The same answer the authorization middleware gives. A different one here would confirm
		// to a caller that a tenant they may not reach exists.
		writeJSON(writer, http.StatusNotFound, errorView{Error: "organization not found"})
	case errors.Is(err, audit.ErrWriteFailed):
		h.Logger.ErrorContext(request.Context(), "an operation was rolled back unrecorded",
			slog.String("path", request.URL.Path),
			slog.String("error", err.Error()))
		writeJSON(writer, http.StatusServiceUnavailable, errorView{
			Error: "the change was refused because it could not be recorded"})
	case errors.Is(err, ErrUnknown):
		writeJSON(writer, http.StatusNotFound, errorView{Error: "incident not found"})
	case errors.Is(err, ErrMerge):
		writeJSON(writer, http.StatusConflict, errorView{Error: err.Error()})
	case errors.Is(err, ErrBadCursor):
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "cursor is not one we issued"})
	default:
		h.Logger.ErrorContext(request.Context(), "an incident request failed",
			slog.String("path", request.URL.Path),
			slog.String("error", err.Error()))
		writeJSON(writer, http.StatusInternalServerError, errorView{Error: "request failed"})
	}
}

func pageOf(request *http.Request) SignalPage {
	page := SignalPage{After: request.URL.Query().Get("cursor")}
	if size, err := strconv.Atoi(request.URL.Query().Get("limit")); err == nil {
		page.Limit = size
	}
	return page
}
