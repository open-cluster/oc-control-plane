package conversation

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/api/sse"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/execution"
)

func (h Handlers) execution(writer http.ResponseWriter, request *http.Request) {
	_, org, chat, id, ok := h.executionAddress(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()
	snapshot, err := h.Store.ChatExecutionSnapshot(ctx, org, chat, id)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, snapshot)
}

func (h Handlers) streamExecution(writer http.ResponseWriter, request *http.Request) {
	_, org, chat, id, ok := h.executionAddress(writer, request)
	if !ok {
		return
	}
	sse.Stream(writer, request, h.StreamContext, h.Resolve, authz.ConversationRead,
		func(ctx context.Context) (execution.Snapshot, error) {
			return h.Store.ChatExecutionSnapshot(ctx, org, chat, id)
		}, h.fail)
}

func (h Handlers) cancelExecution(writer http.ResponseWriter, request *http.Request) {
	principal, org, chat, id, ok := h.executionAddress(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()
	snapshot, err := h.Store.CancelChatExecution(ctx, principal, org, chat, id)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, snapshot)
}

func (h Handlers) executionAddress(writer http.ResponseWriter, request *http.Request) (authz.Principal, uuid.UUID, uuid.UUID, uuid.UUID, bool) {
	principal, org, chat, ok := h.addressed(writer, request)
	if !ok {
		return principal, org, chat, uuid.Nil, false
	}
	id, err := uuid.Parse(request.PathValue("execution"))
	if err != nil || id == uuid.Nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "execution is not an identity"})
		return principal, org, chat, id, false
	}
	return principal, org, chat, id, true
}
