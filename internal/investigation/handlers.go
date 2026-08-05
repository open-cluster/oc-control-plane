package investigation

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
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// The routes a client reads a case through.
//
// They live on the operator surface, which already exists, is already off the public interface, and
// already reads across tenants behind a credential of its own. This package owns what the reads
// mean and what they return; that surface owns who may reach them, and a second copy of that
// decision would be a second place for it to be wrong.
//
// The principal is resolved before any of these run, so a case now records who opened it, who
// cancelled it and who asked for it to be reinvestigated. That is what ADR-006 was open on and
// what internal/authz and internal/identity now answer.

const (
	// readTimeout bounds one read, so a query cannot outlive the attention of whoever made it.
	readTimeout = 15 * time.Second
	// openTimeout is longer: opening a case writes, and a write that times out halfway is worse
	// than one that takes a moment.
	openTimeout = 30 * time.Second
)

// Handlers is this capability's dependencies.
//
// Reads and writes are separate fields rather than one store, because a handler given the writing
// interface is a handler one typo away from mutating what it was asked to display.
type Handlers struct {
	Reader Reader
	Store  Store
	Logger *slog.Logger
	// Controls is what a new round runs under when a customer has authored nothing.
	Controls Controls
	// Versions is what this build pins into every round it opens.
	Versions Versions
}

// Routes is this capability's contribution to the operator API's index.
//
// Reading a case and driving one are separate permissions, which is what makes the Viewer role
// possible: an engineer looped in during an incident sees everything and moves nothing.
func (h Handlers) Routes() authz.Table {
	const base = "/operator/v1/organizations/{organization}/investigations"

	read := func(pattern string, handler http.HandlerFunc) authz.Route {
		return authz.Privileged(http.MethodGet, pattern, authz.InvestigationRead, handler)
	}
	return authz.Table{
		authz.Privileged(http.MethodPost, base, authz.InvestigationOpen,
			http.HandlerFunc(h.open)),
		read(base, h.list),
		read(base+"/{investigation}", h.summary),
		authz.Privileged(http.MethodPost, base+"/{investigation}/cancel",
			authz.InvestigationCancel, http.HandlerFunc(h.cancel)),
		authz.Privileged(http.MethodPost, base+"/{investigation}/reinvestigate",
			authz.InvestigationReopen, http.HandlerFunc(h.reinvestigate)),

		read(base+"/{investigation}/timeline", h.timeline),
		read(base+"/{investigation}/evidence", h.evidence),
		read(base+"/{investigation}/evidence/{evidence}", h.evidenceItem),
		read(base+"/{investigation}/hypotheses", h.hypotheses),
		read(base+"/{investigation}/coverage-gaps", h.gaps),
		read(base+"/{investigation}/coverage", h.coverage),
		read(base+"/{investigation}/activity", h.activity),
		read(base+"/{investigation}/case-file", h.caseFile),
	}
}

// open starts a case from a Connection, a scope and a window, and opens its first round.
//
// Both happen here rather than in a worker, so that the response an engineer gets already names a
// case they can poll. The round is opened unclaimed: a worker picks it up, which is what makes a
// control-plane restart between the two recoverable rather than a lost run.
func (h Handlers) open(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), openTimeout)
	defer cancel()

	var body openRequest
	if !decode(writer, request, &body) {
		return
	}
	wanted, ok := h.plan(writer, body, principal)
	if !ok {
		return
	}

	opened, err := h.Store.OpenInvestigation(ctx, principal, organization, wanted)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	if _, err = h.Store.OpenRound(ctx, principal, organization, Opening{
		InvestigationID: opened.ID,
		Controls:        h.Controls,
		Plan: Plan{
			Template: "kubernetes-workload-v1",
			Intended: openingReads(opened.Scope, opened.Window),
		},
		Versions: h.Versions,
	}); err != nil {
		h.fail(writer, request, err)
		return
	}

	// Identifiers, counts and reasons. Never a scope's contents and never evidence.
	h.Logger.InfoContext(ctx, "investigation opened",
		slog.String("organization", organization.String()),
		slog.String("investigation_id", opened.ID.String()),
		slog.String("environment_id", opened.Environment.String()),
		slog.String("connection_id", opened.Scope.Connection.String()),
		slog.String("caller", h.callerName(request)))

	summary, err := h.Reader.InvestigationSummary(ctx, organization, opened.ID)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	h.stamp(writer, summary.Investigation.CaseVersion)
	writeJSON(writer, http.StatusCreated, summaryViewOf(summary))
}

// plan turns a request into what storage needs, refusing everything that could not work. It answers
// the caller itself on a refusal, so the handler either has a valid plan or has already returned.
func (h Handlers) plan(
	writer http.ResponseWriter, body openRequest, principal authz.Principal,
) (New, bool) {
	connection, err := uuid.Parse(body.ConnectionID)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: "connectionId is not an identity"})
		return New{}, false
	}
	kind, known := ParseWorkloadKind(body.WorkloadKind)
	if !known {
		writeJSON(writer, http.StatusBadRequest, errorView{
			Error: `workloadKind must be "deployment", "statefulset" or "daemonset"`,
		})
		return New{}, false
	}

	wanted := New{
		Scope: Scope{
			Connection:   connection,
			Namespace:    body.Namespace,
			WorkloadKind: kind,
			WorkloadName: body.WorkloadName,
		},
		Window: Window{Start: body.WindowStart, End: body.WindowEnd},
		Trigger: Trigger{
			Kind:        TriggerManual,
			RequestedBy: requestedBy(principal),
			At:          time.Now(),
		},
	}
	if body.EpisodeID != "" {
		episode, parseErr := uuid.Parse(body.EpisodeID)
		if parseErr != nil {
			writeJSON(writer, http.StatusBadRequest,
				errorView{Error: "episodeId is not an identity"})
			return New{}, false
		}
		wanted.EpisodeKey = episode.String()
	}
	if err = wanted.Validate(); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: err.Error()})
		return New{}, false
	}
	return wanted, true
}

// summary answers the one read a client polls, and answers it CHEAPLY when nothing has changed.
//
// A conditional request carrying the case version a client already holds is answered from one
// primary-key read, without touching a single section table. That is what makes polling affordable,
// and it is only possible because the case version advances on any change within the case rather
// than only on a lifecycle transition.
func (h Handlers) summary(writer http.ResponseWriter, request *http.Request) {
	organization, id, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	if held, conditional := heldVersion(request); conditional {
		version, err := h.Reader.CaseVersion(ctx, organization, id)
		if err != nil {
			h.fail(writer, request, err)
			return
		}
		if version == held {
			h.stamp(writer, version)
			writer.WriteHeader(http.StatusNotModified)
			return
		}
	}

	summary, err := h.Reader.InvestigationSummary(ctx, organization, id)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	h.stamp(writer, summary.Investigation.CaseVersion)
	writeJSON(writer, http.StatusOK, summaryViewOf(summary))
}

func (h Handlers) list(writer http.ResponseWriter, request *http.Request) {
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	filter := ListFilter{Running: request.URL.Query().Get("running") == "true"}
	if named := request.URL.Query().Get("environment"); named != "" {
		environment, err := uuid.Parse(named)
		if err != nil {
			writeJSON(writer, http.StatusBadRequest,
				errorView{Error: "environment is not an identity"})
			return
		}
		filter.Environment = environment
	}

	list, err := h.Reader.ListInvestigations(ctx, organization, filter, pageOf(request))
	if err != nil {
		h.fail(writer, request, err)
		return
	}

	// Every read of this surface crosses a tenant boundary, so every read is recorded.
	h.Logger.InfoContext(ctx, "operator read investigations",
		slog.String("organization", organization.String()),
		slog.Int("investigations", len(list.Rows)),
		slog.String("caller", h.callerName(request)))

	rows := make([]rowView, 0, len(list.Rows))
	for _, row := range list.Rows {
		rows = append(rows, rowViewOf(row))
	}
	writeJSON(writer, http.StatusOK, listView{Investigations: rows, Next: list.Next})
}

func (h Handlers) cancel(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, id, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), openTimeout)
	defer cancel()

	if err := h.Store.CancelInvestigation(ctx, principal, organization, id); err != nil {
		h.fail(writer, request, err)
		return
	}
	h.Logger.InfoContext(ctx, "investigation cancelled",
		slog.String("organization", organization.String()),
		slog.String("investigation_id", id.String()),
		slog.String("caller", h.callerName(request)))
	writer.WriteHeader(http.StatusNoContent)
}

// reinvestigate adds a round to a case that has already run.
//
// It is a new ROUND rather than a new case: the identity, the URL and the permalink an engineer
// shared survive, and what they read the first time still exists with its attribution (ADR-013).
func (h Handlers) reinvestigate(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, id, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), openTimeout)
	defer cancel()

	found, err := h.Store.Investigation(ctx, organization, id)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	if _, err = h.Store.OpenRound(ctx, principal, organization, Opening{
		InvestigationID: id,
		Reinvestigation: true,
		Controls:        h.Controls,
		Plan: Plan{
			Template: "kubernetes-workload-v1",
			Intended: openingReads(found.Scope, found.Window),
		},
		Versions: h.Versions,
	}); err != nil {
		h.fail(writer, request, err)
		return
	}
	h.Logger.InfoContext(ctx, "investigation reopened",
		slog.String("organization", organization.String()),
		slog.String("investigation_id", id.String()),
		slog.String("caller", h.callerName(request)))

	summary, err := h.Reader.InvestigationSummary(ctx, organization, id)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	h.stamp(writer, summary.Investigation.CaseVersion)
	writeJSON(writer, http.StatusAccepted, summaryViewOf(summary))
}

// section is the shape every paginated read shares: resolve the tenant and the case, read, stamp
// the page with the version it represents, and answer.
func (h Handlers) section(
	writer http.ResponseWriter, request *http.Request,
	read func(context.Context, tenancy.Organization, uuid.UUID) (any, int64, error),
) {
	organization, id, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	body, version, err := read(ctx, organization, id)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	h.stamp(writer, version)
	writeJSON(writer, http.StatusOK, body)
}

// renderSection maps a section's items through a view function, keeping the page's version and
// cursor. It is generic because every section is the same shape and writing six copies of it is how
// one of them ends up not carrying the version.
func renderSection[T any, V any](section Section[T], view func(T) V) sectionView[V] {
	rendered := sectionView[V]{
		Items:       make([]V, 0, len(section.Items)),
		Next:        section.Next,
		CaseVersion: section.CaseVersion,
	}
	for _, item := range section.Items {
		rendered.Items = append(rendered.Items, view(item))
	}
	return rendered
}

// stamp puts the case version on the response as an entity tag, which is what a conditional request
// sends back. It is the version and nothing else: a tag derived from the body would change when the
// rendering changed, and a client would refetch a case that had not moved.
func (h Handlers) stamp(writer http.ResponseWriter, version int64) {
	writer.Header().Set("ETag", `"`+strconv.FormatInt(version, 10)+`"`)
}

// heldVersion reads the case version a client already holds, from a conditional request.
func heldVersion(request *http.Request) (int64, bool) {
	tag := request.Header.Get("If-None-Match")
	if len(tag) < 3 || tag[0] != '"' || tag[len(tag)-1] != '"' {
		return 0, false
	}
	version, err := strconv.ParseInt(tag[1:len(tag)-1], 10, 64)
	if err != nil {
		return 0, false
	}
	return version, true
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
	id, err := uuid.Parse(request.PathValue("investigation"))
	if err != nil {
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: "investigation is not an identity"})
		return tenancy.Organization{}, uuid.UUID{}, false
	}
	return organization, id, true
}

// fail answers an error, naming the ones a caller can act on.
//
// A case this organization does not have and a case that is another organization's are ONE answer.
// Telling them apart would let a caller compose an organization from one path and an investigation
// from another until one of the pairs landed, which is the exact composition the tenant boundary
// exists to refuse.
func (h Handlers) fail(writer http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, authz.ErrNotAMember):
		// The same answer the authorization middleware gives. A different one here would
		// confirm to a caller that a tenant they may not reach exists.
		writeJSON(writer, http.StatusNotFound, errorView{Error: "organization not found"})
	case errors.Is(err, audit.ErrWriteFailed):
		h.Logger.ErrorContext(request.Context(), "an operation was rolled back unrecorded",
			slog.String("path", request.URL.Path),
			slog.String("error", err.Error()))
		writeJSON(writer, http.StatusServiceUnavailable, errorView{
			Error: "the change was refused because it could not be recorded"})
	case errors.Is(err, ErrUnknown), errors.Is(err, ErrRoundUnknown):
		writeJSON(writer, http.StatusNotFound, errorView{Error: "investigation not found"})
	case errors.Is(err, ErrConnectionUnusable):
		writeJSON(writer, http.StatusNotFound, errorView{
			Error: "the connection named is not one this organization can investigate through",
		})
	case errors.Is(err, ErrEpisodeUnusable):
		writeJSON(writer, http.StatusNotFound, errorView{
			Error: "the incident named is not one this organization can investigate, or it is " +
				"in a different environment from the connection this case would read through",
		})
	case errors.Is(err, ErrEpisodeInvestigated):
		// The existing case is NAMED. An operator who asked for an investigation of an incident
		// that already has one wants to be sent to it, and a bare refusal would leave them
		// searching for a case they were just told exists.
		writeJSON(writer, http.StatusConflict, errorView{Error: err.Error()})
	case errors.Is(err, ErrAlreadyTerminal):
		writeJSON(writer, http.StatusConflict,
			errorView{Error: "this investigation has already finished"})
	case errors.Is(err, ErrScope):
		writeJSON(writer, http.StatusBadRequest, errorView{Error: err.Error()})
	case errors.Is(err, ErrCaseMoved):
		writeJSON(writer, http.StatusConflict, errorView{
			Error: "the case has moved past the version asked for; read its current version first",
		})
	case errors.Is(err, ErrCaseTooLarge):
		writeJSON(writer, http.StatusRequestEntityTooLarge, errorView{
			Error: "this case is larger than one assembly may carry; read it by section",
		})
	case errors.Is(err, ErrBadCursor):
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: "after is not a page position from a previous response"})
	default:
		// The error is logged and not returned. It may name a column, a constraint or a placement,
		// and none of those is a caller's to learn.
		h.Logger.ErrorContext(request.Context(), "investigation request failed",
			slog.String("path", request.URL.Path),
			slog.String("error", err.Error()))
		writeJSON(writer, http.StatusInternalServerError, errorView{Error: "request failed"})
	}
}

// callerName is who acted, for the log lines. The record itself is written by storage from the
// principal, in the transaction of the change it describes; this is the line an on-call
// engineer greps.
func (h Handlers) callerName(request *http.Request) string {
	principal, ok := authz.Of(request)
	if !ok {
		return request.RemoteAddr
	}
	return principal.DisplayName() + " (" + request.RemoteAddr + ")"
}

// requestedBy is what a case records as having asked for it. It is the principal's identifier
// rather than their name: a case read months later must resolve to a person even if they have
// since been renamed.
func requestedBy(principal authz.Principal) string {
	if principal.ID() == "" {
		return principal.DisplayName()
	}
	return principal.ID()
}

// caller resolves the principal the guard put on this request. A handler behind the guard
// always has one; its absence is a route mounted outside the permission table, which is a
// programming error answered with a 500 and a log line rather than a panic.
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

func pageOf(request *http.Request) Page {
	page := Page{After: request.URL.Query().Get("after")}
	if size, err := strconv.Atoi(request.URL.Query().Get("limit")); err == nil {
		page.Limit = size
	}
	return page
}
