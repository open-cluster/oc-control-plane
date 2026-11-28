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
	"github.com/open-cluster/oc-control-plane/internal/correlation"
	observability "github.com/open-cluster/oc-control-plane/internal/telemetry"
)

const (
	readTimeout     = 15 * time.Second
	maxRequestBytes = 32 << 10
)

type Handlers struct {
	Store          Store
	Logger         *slog.Logger
	AgentAvailable bool
	Resolve        func(*http.Request) (authz.Principal, error)
	StreamContext  context.Context
}

func (h Handlers) Routes() []authz.Route {
	const base = "/api/v1/conversations"
	const execution = base + "/{conversation}/executions/{execution}"
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
			Method:     http.MethodPatch,
			Pattern:    base + "/{conversation}",
			Permission: authz.ConversationWrite,
			Handler:    http.HandlerFunc(h.rename),
		},
		{
			Method:     http.MethodDelete,
			Pattern:    base + "/{conversation}",
			Permission: authz.ConversationWrite,
			Handler:    http.HandlerFunc(h.remove),
		},
		{
			Method:     http.MethodGet,
			Pattern:    base + "/{conversation}/turns",
			Permission: authz.ConversationRead,
			Handler:    http.HandlerFunc(h.turns),
		},
		{
			Method:     http.MethodGet,
			Pattern:    base + "/{conversation}/messages",
			Permission: authz.ConversationRead,
			Handler:    http.HandlerFunc(h.messages),
		},
		{
			Method:     http.MethodPost,
			Pattern:    base + "/{conversation}/messages",
			Permission: authz.ConversationWrite,
			Handler:    http.HandlerFunc(h.say),
		},
		{
			Method:     http.MethodGet,
			Pattern:    execution,
			Permission: authz.ConversationRead,
			Handler:    http.HandlerFunc(h.readExecution),
		},
		{
			Method:     http.MethodGet,
			Pattern:    execution + "/events",
			Permission: authz.ConversationRead,
			Handler:    http.HandlerFunc(h.streamExecution),
		},
		{
			Method:     http.MethodPost,
			Pattern:    execution + "/cancel",
			Permission: authz.ConversationWrite,
			Handler:    http.HandlerFunc(h.cancelExecution),
		},
		{
			Method:     http.MethodPost,
			Pattern:    execution + "/input",
			Permission: authz.ConversationWrite,
			Handler:    http.HandlerFunc(h.answerExecutionInput),
		},
	}
}

type openRequest struct {
	IncidentID string `json:"incidentId"`
	Title      string `json:"title"`
}

func (h Handlers) open(writer http.ResponseWriter, request *http.Request) {

	principal, organization := h.caller(request)
	var asked openRequest
	if !h.decode(writer, request, &asked) {
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

	title := strings.TrimSpace(asked.Title)
	if len([]rune(title)) > MaxTitleLength {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "the title must be at most 512 characters"})
		return
	}

	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	openedConversation, err := h.Store.OpenConversation(ctx, principal, organization, NewConversation{
		IncidentID: incidentID,
		Source:     SourceWeb,
		Title:      title,
		CreatedBy:  principal.UserID().String(),
	})
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	observability.LoggerFor(ctx, h.Logger, correlation.From(ctx)).InfoContext(ctx, "conversation opened",
		slog.String("org_id", organization.String()),
		slog.String("conversation_id", openedConversation.ID.String()))

	writeJSON(writer, http.StatusCreated, conversationViewOf(openedConversation))
}

type sayRequest struct {
	WindowInput
	ClientMessageID uuid.UUID `json:"clientMessageId"`
	Text            string    `json:"text"`
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
	window, err := asked.Parse(time.Now())
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: err.Error()})
		return
	}
	text := strings.TrimSpace(asked.Text)
	switch {
	case asked.ClientMessageID == uuid.Nil:
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "give a clientMessageId"})
		return
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

	accepted, err := h.Store.AdmitConversationMessage(ctx, principal, organization, id, asked.ClientMessageID, NewMessage{
		Text:   text,
		Window: window,
	}, !h.AgentAvailable)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, messageAcceptedView{
		Message:     messageViewOf(accepted.Message),
		ExecutionID: accepted.ExecutionID,
		Status:      accepted.Status,
		EventsURL:   "/api/v1/conversations/" + id.String() + "/executions/" + accepted.ExecutionID.String() + "/events",
	})
}

var listSpec = listing.Spec{
	Searchable:  true,
	Sortable:    []string{"lastActivityAt"},
	DefaultSort: listing.Sort{Field: "lastActivityAt", Descending: true},
	Filters:     []string{"incidentId"},
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
		Limit:      parsed.Limit,
		After:      parsed.Cursor,
		Search:     parsed.Search,
		Sort:       parsed.Sort.Field,
		Descending: parsed.Sort.Descending,
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

	found, err := h.Store.WebConversation(ctx, organization, id)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, conversationViewOf(found))
}

func (h Handlers) remove(writer http.ResponseWriter, request *http.Request) {
	principal, org, id, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()
	if err := h.Store.DeleteConversation(ctx, principal, org, id); err != nil {
		h.fail(writer, request, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
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
			errorView{
				Error: "the request body is not what this operation accepts",
			})
		return false
	}
	return true
}

func (h Handlers) fail(writer http.ResponseWriter, request *http.Request, err error) {
	logger := observability.LoggerFor(request.Context(), h.Logger, correlation.From(request.Context())).With(
		slog.String("org_id", authz.MustPrincipal(request.Context()).Organization().String()),
		slog.String("method", request.Method))
	if id, parseErr := uuid.Parse(request.PathValue("conversation")); parseErr == nil {
		logger = logger.With(slog.String("conversation_id", id.String()))
	}
	switch {
	case errors.Is(err, ErrInvalidWindow), errors.Is(err, ErrInvalidInputAnswer):
		writeJSON(writer, http.StatusBadRequest, errorView{Error: err.Error()})
	case errors.Is(err, authz.ErrNotAMember):
		writeJSON(writer, http.StatusNotFound, errorView{Error: "organization not found"})
	case errors.Is(err, ErrUnknown):
		writeJSON(writer, http.StatusNotFound, errorView{Error: "conversation not found"})
	case errors.Is(err, ErrIncidentUnknown):
		writeJSON(writer, http.StatusNotFound, errorView{Error: "incident not found"})
	case errors.Is(err, ErrClientMessageConflict), errors.Is(err, ErrLegacyHistory), errors.Is(err, ErrInputConflict):
		writeJSON(writer, http.StatusConflict, errorView{Error: err.Error()})
	case errors.Is(err, ErrChatUnavailable):
		writeJSON(writer, http.StatusServiceUnavailable, errorView{Error: err.Error()})
	case errors.Is(err, ErrQueueFull):
		writeJSON(writer, http.StatusTooManyRequests, errorView{
			Error: "this Organization already has its limit of Messages waiting"})
	case errors.Is(err, ErrBadCursor):
		writeJSON(writer, http.StatusBadRequest, errorView{Error: ErrBadCursor.Error()})
	case errors.Is(err, audit.ErrWriteFailed):
		logger.ErrorContext(request.Context(), "mutation did not commit because audit recording failed",
			slog.String("resource", "conversation"),
			slog.String("error", "audit record could not be written"))
		writeJSON(writer, http.StatusServiceUnavailable, errorView{
			Error: "the change was refused because it could not be recorded"})
	default:
		logger.ErrorContext(request.Context(), "conversation request failed",
			slog.String("resource", "conversation"),
			slog.String("error", "storage operation failed"))
		writeJSON(writer, http.StatusInternalServerError, errorView{Error: "request failed"})
	}
}
