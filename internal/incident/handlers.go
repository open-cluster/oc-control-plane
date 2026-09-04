package incident

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/api/listing"
	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
)

const (
	readTimeout  = 15 * time.Second
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
	const base = "/api/v1/incidents"

	return authz.Table{
		authz.Privileged(http.MethodGet, base, authz.IncidentRead,
			http.HandlerFunc(h.list)),
		authz.Privileged(http.MethodGet, base+"/{incident}", authz.IncidentRead,
			http.HandlerFunc(h.incident)),
		authz.Privileged(http.MethodGet, base+"/{incident}/alert-events", authz.IncidentRead,
			http.HandlerFunc(h.alertEvents)),
		authz.Privileged(http.MethodPost, base+"/{incident}/merge", authz.IncidentMerge,
			http.HandlerFunc(h.merge)),
	}
}

var incidentsSpec = listing.Spec{
	Searchable:  true,
	Sortable:    []string{"lastSeenAt", "firstSeenAt", "title", "alertEventCount"},
	DefaultSort: listing.Sort{Field: "lastSeenAt", Descending: true},
	Filters:     []string{"integrationId", "status"},
}

func (h Handlers) list(writer http.ResponseWriter, request *http.Request) {
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	parsedQuery, ok := h.query(writer, request, incidentsSpec)
	if !ok {
		return
	}
	narrowed, ok := h.narrowing(writer, parsedQuery)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	page, err := h.Store.QueryIncidents(ctx, organization, narrowed)
	if err != nil {
		h.fail(writer, request, err)
		return
	}

	views := make([]incidentView, 0, len(page.Incidents))
	for _, found := range page.Incidents {
		views = append(views, viewOf(found))
	}
	writeJSON(writer, http.StatusOK, listing.Answer(views, page.Next, nil))
}

func (h Handlers) incident(writer http.ResponseWriter, request *http.Request) {
	organization, id, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	found, err := h.Store.Incident(ctx, organization, id)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, viewOf(found))
}

func (h Handlers) alertEvents(writer http.ResponseWriter, request *http.Request) {
	query, ok := h.query(writer, request, listing.Spec{
		DefaultSort: listing.Sort{Field: "startedAt"},
	})
	if !ok {
		return
	}
	organization, id, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	list, err := h.Store.IncidentAlertEvents(ctx, organization, id, AlertEventPage{
		Limit: query.Limit, After: query.Cursor,
	})
	if err != nil {
		h.fail(writer, request, err)
		return
	}

	views := make([]alertEventView, 0, len(list.AlertEvents))
	for _, found := range list.AlertEvents {
		views = append(views, alertEventViewOf(found))
	}
	writeJSON(writer, http.StatusOK, listing.Answer(views, list.Next, nil))
}

// merge records that two incidents an operator is looking at are one incident.
//
// The incident in the PATH is the one that gives way, and the body names the one that survives. That
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
			errorView{Error: "into is not an incident identity"})
		return
	}

	wanted := Merge{Absorbed: id, Into: into, Reason: body.Reason}
	if err = wanted.Validate(); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(request.Context(), mergeTimeout)
	defer cancel()

	surviving, err := h.Store.MergeIncidents(ctx, principal, organization, wanted)
	if err != nil {
		h.fail(writer, request, err)
		return
	}

	// Identifiers and counts. Never a title and never a AlertEvent's text: both are what a customer's
	// systems produced and a log that quoted them would turn diagnosis into a disclosure channel.
	h.Logger.InfoContext(ctx, "incident incidents merged",
		slog.String("organization", organization.String()),
		slog.String("absorbed_incident_id", id.String()),
		slog.String("surviving_incident_id", into.String()),
		slog.String("caller", h.callerName(request)))

	writeJSON(writer, http.StatusOK, viewOf(surviving))
}

func (h Handlers) narrowing(writer http.ResponseWriter, query listing.Query) (Query, bool) {
	narrowed := Query{
		Search:     query.Search,
		Sort:       query.Sort.Field,
		Descending: query.Sort.Descending,
		Cursor:     query.Cursor,
		Limit:      query.Limit,
	}

	if named := query.Filter("integrationId"); named != "" {
		integration, err := uuid.Parse(named)
		if err != nil {
			writeJSON(writer, http.StatusBadRequest,
				errorView{Error: "integrationId is not an identity"})
			return Query{}, false
		}
		narrowed.Integration = &integration
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
	writer http.ResponseWriter,
	request *http.Request,
	spec listing.Spec,
) (listing.Query, bool) {
	parsed, err := listing.Parse(request.URL.Query(), spec)
	if err != nil {
		if listing.Refused(err) {
			writeJSON(writer, http.StatusBadRequest, errorView{Error: err.Error()})
			return listing.Query{}, false
		}
		h.Logger.ErrorContext(request.Context(), "a listing declares a query it cannot serve",
			slog.String("path", request.URL.Path),
			slog.String("error", err.Error()))
		writeJSON(writer, http.StatusInternalServerError, errorView{Error: "request failed"})
		return listing.Query{}, false
	}
	return parsed, true
}

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

func (h Handlers) addressed(
	writer http.ResponseWriter, request *http.Request,
) (tenancy.Organization, uuid.UUID, bool) {
	organization, ok := h.organization(writer, request)
	if !ok {
		return tenancy.Organization{}, uuid.UUID{}, false
	}
	id, err := uuid.Parse(request.PathValue("incident"))
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "incident is not an identity"})
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
// An incident this organization does not have and one that is another organization's are ONE
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
