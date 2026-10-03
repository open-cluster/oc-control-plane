package slack

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

type RecoveryHandlers struct {
	Database *storage.Database
	Logger   *slog.Logger
}

func (h RecoveryHandlers) Routes() []authz.Route {
	return []authz.Route{{
		Method:     http.MethodPost,
		Pattern:    "/api/v1/slack/conversations/{conversation}/messages/{sequence}/recover",
		Permission: authz.SlackMessageRecover,
		Handler:    http.HandlerFunc(h.recover),
	}}
}

func (h RecoveryHandlers) recover(writer http.ResponseWriter, request *http.Request) {
	conversationID, err := uuid.Parse(request.PathValue("conversation"))
	if err != nil {
		writeRecoveryJSON(writer, http.StatusBadRequest, "conversation is not an identity")
		return
	}
	sequence, err := strconv.ParseInt(request.PathValue("sequence"), 10, 64)
	if err != nil || sequence < 1 {
		writeRecoveryJSON(writer, http.StatusBadRequest, "sequence must be a positive integer")
		return
	}
	principal := authz.MustPrincipal(request.Context())
	organization := principal.Organization()
	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	err = h.Database.RecoverSlackMessage(ctx, principal, organization, conversationID, sequence)
	switch {
	case errors.Is(err, storage.ErrSlackMessageRecoveryUnavailable):
		writeRecoveryJSON(writer, http.StatusConflict, "only terminal Slack Messages can be recovered")
	case err != nil:
		if h.Logger != nil {
			h.Logger.ErrorContext(ctx, "Slack Message recovery failed", slog.String("error", err.Error()))
		}
		writeRecoveryJSON(writer, http.StatusInternalServerError, "request failed")
	default:
		writer.WriteHeader(http.StatusNoContent)
	}
}

func writeRecoveryJSON(writer http.ResponseWriter, status int, message string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]string{"error": message})
}
