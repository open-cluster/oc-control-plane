package conversation

import (
	"context"
	"net/http"
	"strings"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/api/sse"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/execution"
)

func (h Handlers) readExecution(writer http.ResponseWriter, request *http.Request) {
	principal, org, conversationID, executionID, ok := h.executionAddress(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()
	snapshot, err := h.Store.ChatExecutionSnapshot(ctx, principal, org, conversationID, executionID)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, snapshot)
}

func (h Handlers) streamExecution(writer http.ResponseWriter, request *http.Request) {
	principal, org, conversationID, executionID, ok := h.executionAddress(writer, request)
	if !ok {
		return
	}
	sse.Stream(writer, request, h.StreamContext, h.Resolve, authz.ConversationRead,
		func(ctx context.Context) (execution.Snapshot, error) {
			return h.Store.ChatExecutionSnapshot(ctx, principal, org, conversationID, executionID)
		}, h.fail)
}

func (h Handlers) cancelExecution(writer http.ResponseWriter, request *http.Request) {
	principal, org, conversationID, executionID, ok := h.executionAddress(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()
	snapshot, err := h.Store.CancelChatExecution(ctx, principal, org, conversationID, executionID)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, snapshot)
}

func (h Handlers) executionAddress(writer http.ResponseWriter, request *http.Request) (authz.Principal, uuid.UUID, uuid.UUID, uuid.UUID, bool) {
	principal, org, conversationID, ok := h.addressed(writer, request)
	if !ok {
		return principal, org, conversationID, uuid.Nil, false
	}
	executionID, err := uuid.Parse(request.PathValue("execution"))
	if err != nil || executionID == uuid.Nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "execution is not an identity"})
		return principal, org, conversationID, executionID, false
	}
	return principal, org, conversationID, executionID, true
}

func (h Handlers) answerExecutionInput(writer http.ResponseWriter, request *http.Request) {
	principal, org, conversationID, executionID, ok := h.executionAddress(writer, request)
	if !ok {
		return
	}
	occurrence, err := uuid.Parse(request.PathValue("occurrence"))
	if err != nil || occurrence == uuid.Nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "occurrence is not an identity"})
		return
	}
	keys := request.Header.Values("Idempotency-Key")
	if len(keys) != 1 || strings.TrimSpace(keys[0]) == "" || len(keys[0]) > 128 {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "give one Idempotency-Key of at most 128 bytes"})
		return
	}
	var answer tool.AskUserMetadata
	if !h.decode(writer, request, &answer) {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()
	if err := h.Store.AnswerChatInput(ctx, principal, org, conversationID, executionID, occurrence, strings.TrimSpace(keys[0]), answer); err != nil {
		h.fail(writer, request, err)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(http.StatusAccepted)
}
