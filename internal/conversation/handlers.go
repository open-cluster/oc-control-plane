package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/api/listing"
	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
)

const (
	readTimeout      = 15 * time.Second
	maxRequestBytes  = 32 << 10
	transcriptWindow = 100
)

type Handlers struct {
	Store           Store
	Logger          *slog.Logger
	WindowLead      time.Duration
	MaxWaitingTurns int
}

func (h Handlers) Routes() []authz.Route {
	const base = "/api/v1/conversations"
	return []authz.Route{
		{
			Method:     http.MethodGet,
			Pattern:    base,
			Permission: authz.ConversationRead,
			Handler:    http.HandlerFunc(h.list),
		},
		{
			Method:     http.MethodPost,
			Pattern:    base,
			Permission: authz.ConversationWrite,
			Handler:    http.HandlerFunc(h.open),
		},
		{
			Method:     http.MethodGet,
			Pattern:    base + "/{conversation}",
			Permission: authz.ConversationRead,
			Handler:    http.HandlerFunc(h.read),
		},
		{
			Method:     http.MethodGet,
			Pattern:    base + "/{conversation}/turns",
			Permission: authz.ConversationRead,
			Handler:    http.HandlerFunc(h.turns),
		},
		{
			Method:     http.MethodPost,
			Pattern:    base + "/{conversation}/messages",
			Permission: authz.ConversationWrite,
			Handler:    http.HandlerFunc(h.say),
		},
	}
}

type openRequest struct {
	windowInput
	IncidentID string `json:"incidentId"`
	Subject    string `json:"subject"`
	Message    string `json:"message"`
}

func (h Handlers) open(writer http.ResponseWriter, request *http.Request) {
	principal, organization := h.caller(request)
	var asked openRequest
	if !h.decode(writer, request, &asked) {
		return
	}
	window, err := asked.parse(time.Now())
	if err != nil || (window != nil && strings.TrimSpace(asked.Message) == "") {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: ErrInvalidWindow.Error()})
		return
	}

	incidentID := uuid.Nil
	if trimmed := strings.TrimSpace(asked.IncidentID); trimmed != "" {
		parsed, err := uuid.Parse(trimmed)
		if err != nil {
			writeJSON(writer, http.StatusBadRequest,
				errorView{Error: "incidentId is not an identity"})
			return
		}
		incidentID = parsed
	}

	subject := strings.TrimSpace(asked.Subject)
	switch {
	case subject == "":
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: "give a subject for the conversation"})
		return
	case len([]rune(subject)) > MaxSubjectLength:
		writeJSON(writer, http.StatusBadRequest, errorView{
			Error: "the subject must be at most 512 characters"})
		return
	}

	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	opened, err := h.Store.OpenConversation(ctx, principal, organization, NewConversation{
		IncidentID: incidentID,
		Surface:    SurfaceWeb,
		Subject:    subject,
		CreatedBy:  principal.UserID().String(),
	})
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	h.Logger.InfoContext(ctx, "conversation opened",
		slog.String("org_id", organization.String()),
		slog.String("conversation_id", opened.ID.String()))

	if strings.TrimSpace(asked.Message) == "" {
		writeJSON(writer, http.StatusCreated, conversationViewOf(opened))
		return
	}

	said, turn, queued, err := h.append(ctx, principal, organization, opened.ID,
		asked.Message, window)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusCreated, openedView{
		conversationView: conversationViewOf(opened),
		messageAcceptedView: messageAcceptedView{
			Message: messageViewOf(said),
			Turn:    turn,
			Queued:  queued,
		},
	})
}

type sayRequest struct {
	windowInput
	Message string `json:"message"`
}

func (h Handlers) say(writer http.ResponseWriter, request *http.Request) {
	principal, organization, id, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	var asked sayRequest
	if !h.decode(writer, request, &asked) {
		return
	}
	window, err := asked.parse(time.Now())
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: err.Error()})
		return
	}
	text := strings.TrimSpace(asked.Message)
	switch {
	case text == "":
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "give a message"})
		return
	case len([]rune(text)) > MaxMessageTextLength:
		writeJSON(writer, http.StatusBadRequest, errorView{
			Error: "the message must be at most 8192 characters"})
		return
	}

	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	said, turn, queued, err := h.append(ctx, principal, organization, id, text, window)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, messageAcceptedView{
		Message: messageViewOf(said),
		Turn:    turn,
		Queued:  queued,
	})
}

func (h Handlers) append(ctx context.Context,
	principal authz.Principal,
	organization uuid.UUID,
	id uuid.UUID,
	text string,
	window *Window,
) (Message, *turnView, bool, error) {
	said, turn, opened, err := h.Store.AppendMessageAndOpenTurn(ctx, principal, organization, id, NewMessage{
		Role:         RolePerson,
		ActorKind:    ActorPrincipal,
		ActorID:      boundedRunes(principal.UserID().String(), MaxActorIDLength),
		ActorDisplay: boundedRunes(principal.Actor().DisplayName, MaxActorDisplayLength),
		Text:         text,
		Window:       window,
	}, h.WindowLead, h.MaxWaitingTurns)
	if err != nil {
		return Message{}, nil, false, err
	}
	if !opened {
		return said, nil, true, nil
	}
	rendered := turnViewOf(turn)
	return said, &rendered, false, nil
}

var listSpec = listing.Spec{
	Searchable:  true,
	Sortable:    []string{"lastActivityAt"},
	DefaultSort: listing.Sort{Field: "lastActivityAt", Descending: true},
	Filters:     []string{"incidentId", "state"},
}

func (h Handlers) list(writer http.ResponseWriter, request *http.Request) {
	principal, organization := h.caller(request)
	parsed, err := listing.Parse(request.URL.Query(), listSpec)
	if err != nil {
		if listing.Refused(err) {
			writeJSON(writer, http.StatusBadRequest, errorView{Error: err.Error()})
			return
		}
		h.Logger.ErrorContext(request.Context(),
			"the conversations listing declares a query it cannot serve",
			slog.String("error", err.Error()))
		writeJSON(writer, http.StatusInternalServerError, errorView{Error: "request failed"})
		return
	}

	page := Page{
		Limit: parsed.Limit, After: parsed.Cursor, Search: parsed.Search,
		Sort: parsed.Sort.Field, Descending: parsed.Sort.Descending,
	}
	if incident := parsed.Filter("incidentId"); incident != "" {
		id, parseErr := uuid.Parse(incident)
		if parseErr != nil {
			writeJSON(writer, http.StatusBadRequest,
				errorView{Error: "incidentId is not an identity"})
			return
		}
		page.Incident = id
	}
	switch parsed.Filter("state") {
	case "":
	case "open":
		page.State = StateOpen
	case "closed":
		page.State = StateClosed
	default:
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: "state is open or closed"})
		return
	}

	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	listed, err := h.Store.QueryConversations(ctx, principal, organization, page)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	views := make([]conversationView, 0, len(listed.Conversations))
	for _, found := range listed.Conversations {
		views = append(views, conversationViewOf(found))
	}
	writeJSON(writer, http.StatusOK, listing.NewPage(views, listed.Next, nil))
}

func (h Handlers) read(writer http.ResponseWriter, request *http.Request) {
	_, organization, id, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	detail, err := h.Store.ConversationDetail(ctx, organization, id, transcriptWindow)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, detailViewOf(detail))
}

func (h Handlers) caller(request *http.Request) (authz.Principal, uuid.UUID) {
	principal := authz.MustPrincipal(request.Context())
	return principal, principal.Organization()
}

func (h Handlers) addressed(writer http.ResponseWriter, request *http.Request,
) (authz.Principal, uuid.UUID, uuid.UUID, bool) {

	principal, organization := h.caller(request)
	id, err := uuid.Parse(request.PathValue("conversation"))
	if err != nil {
		writeJSON(writer, http.StatusBadRequest,
			errorView{
				Error: "conversation is not an identity",
			})
		return authz.Principal{}, uuid.UUID{}, uuid.UUID{}, false
	}
	return principal, organization, id, true
}

func (h Handlers) decode(
	writer http.ResponseWriter, request *http.Request, into any,
) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, maxRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: "the request body is not what this operation accepts"})
		return false
	}
	return true
}

func (h Handlers) fail(writer http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, ErrInvalidWindow):
		writeJSON(writer, http.StatusBadRequest, errorView{Error: err.Error()})
	case errors.Is(err, ErrWindowConflict):
		writeJSON(writer, http.StatusConflict, errorView{Error: err.Error()})
	case errors.Is(err, authz.ErrNotAMember):
		writeJSON(writer, http.StatusNotFound, errorView{Error: "organization not found"})
	case errors.Is(err, ErrUnknown):
		writeJSON(writer, http.StatusNotFound, errorView{Error: "conversation not found"})
	case errors.Is(err, ErrIncidentUnknown):
		writeJSON(writer, http.StatusNotFound, errorView{Error: "incident not found"})
	case errors.Is(err, ErrClosed):
		writeJSON(writer, http.StatusConflict, errorView{
			Error: "this conversation is closed; open a new one"})
	case errors.Is(err, ErrQueueFull):
		writeJSON(writer, http.StatusTooManyRequests, errorView{
			Error: "this organization already has its limit of investigations waiting; " +
				"wait for one to end and send this again"})
	case errors.Is(err, ErrBadCursor):
		writeJSON(writer, http.StatusBadRequest, errorView{Error: ErrBadCursor.Error()})
	case errors.Is(err, audit.ErrWriteFailed):
		h.Logger.ErrorContext(request.Context(), "an operation was rolled back unrecorded",
			slog.String("path", request.URL.Path),
			slog.String("error", err.Error()))
		writeJSON(writer, http.StatusServiceUnavailable, errorView{
			Error: "the change was refused because it could not be recorded"})
	default:
		h.Logger.ErrorContext(request.Context(), "conversation request failed",
			slog.String("path", request.URL.Path),
			slog.String("error", err.Error()))
		writeJSON(writer, http.StatusInternalServerError, errorView{Error: "request failed"})
	}
}
