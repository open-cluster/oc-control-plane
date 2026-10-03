package webhooks

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/conversation"
	"github.com/open-cluster/oc-control-plane/internal/correlation"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/integrations/slack"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

const SlackEventsPath = "/webhooks/v1/slack/events"

const slackEventsRoute = "POST " + SlackEventsPath

type SlackAgent struct {
	SigningSecret   string
	Enabled         func(uuid.UUID) bool
	WindowLead      time.Duration
	MaxWaitingTurns int
}

func (s *SlackAgent) Serves() bool { return s != nil && s.SigningSecret != "" }

func (h *receiver) handleSlackEvents(writer http.ResponseWriter, request *http.Request) {
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()
	requestID := correlation.From(ctx)

	began := time.Now()
	defer func() { h.counters.observeSlackAcknowledgement(ctx, time.Since(began)) }()

	body, err := readBody(writer, request)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.counters.countRequest(ctx, surfaceSlack, resultRejected)
			writeStatus(writer, http.StatusRequestEntityTooLarge, "payload too large")
			return
		}
		h.counters.countRequest(ctx, surfaceSlack, resultRejected)
		writeStatus(writer, http.StatusBadRequest, "payload not received")
		return
	}

	if err := slack.Verify(h.Slack.SigningSecret, request.Header.Get(slack.SignatureHeader),
		request.Header.Get(slack.TimestampHeader), body, time.Now()); err != nil {
		h.Logger.WarnContext(ctx, "a slack events request was refused",
			slog.String("request_id", requestID),
			slog.String("caller", callerOf(request)),
			slog.String("reason", err.Error()))
		h.counters.countRequest(ctx, surfaceSlack, resultRejected)
		writeStatus(writer, http.StatusUnauthorized, "unauthorized")
		return
	}

	envelope, err := slack.Parse(body)
	if err != nil {
		h.counters.countRequest(ctx, surfaceSlack, resultRejected)
		writeStatus(writer, http.StatusBadRequest, "payload not understood")
		return
	}

	if envelope.Challenge != "" {
		h.counters.countRequest(ctx, surfaceSlack, resultAccepted)
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Cache-Control", "no-store")
		writer.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(writer).Encode(map[string]string{"challenge": envelope.Challenge})
		return
	}

	integration, routing, err := h.Database.IntegrationByInstallation(ctx,
		"slack", integrations.InstallationKey(envelope.Key()))
	if err != nil {
		if errors.Is(err, integrations.ErrUnknown) {
			h.counters.countRequest(ctx, surfaceSlack, resultRejected)
			writeStatus(writer, http.StatusUnauthorized, "unauthorized")
			return
		}
		h.Logger.ErrorContext(ctx, "could not resolve a slack installation",
			slog.String("request_id", requestID),
			slog.String("error", err.Error()))
		h.counters.countRequest(ctx, surfaceSlack, resultError)
		writeStatus(writer, http.StatusServiceUnavailable, "not recorded")
		return
	}

	organization, err := uuid.Parse(strings.TrimSpace(integration.OrgID))
	if err != nil || organization == uuid.Nil {
		if err == nil {
			err = errors.New("invalid organization identifier")
		}
		h.Logger.ErrorContext(ctx, "a slack installation names an organization that is not a name",
			slog.String("request_id", requestID),
			slog.String("integration_id", integration.ID.String()),
			slog.String("error", err.Error()))
		h.counters.countRequest(ctx, surfaceSlack, resultError)
		writeStatus(writer, http.StatusServiceUnavailable, "not recorded")
		return
	}

	switch {
	case integration.Disabled:
		h.counters.countRequest(ctx, surfaceSlack, resultAccepted)
		writeStatus(writer, http.StatusOK, "ignored")
		return
	case h.Slack.Enabled == nil || !h.Slack.Enabled(organization):
		h.counters.countRequest(ctx, surfaceSlack, resultAccepted)
		writeStatus(writer, http.StatusOK, "ignored")
		return
	case !envelope.AddressedToUs(routing.ProviderActorID):
		h.counters.countRequest(ctx, surfaceSlack, resultAccepted)
		writeStatus(writer, http.StatusOK, "ignored")
		return
	}

	h.acceptSlackMessage(ctx, writer, organization, integration.ID, requestID, body, envelope)
}

func (h *receiver) acceptSlackMessage(
	ctx context.Context, writer http.ResponseWriter, organization uuid.UUID,
	integration uuid.UUID, requestID string, body []byte, envelope slack.Envelope,
) {
	digest := sha256.Sum256(body)
	outcome, err := h.Database.RecordSlackMessage(ctx, organization, storage.SlackMessage{
		Integration:   integration,
		ContentDigest: digest[:],
		Channel:       envelope.Event.Channel,
		Thread:        envelope.Thread(),
		MessageID:     envelope.Event.TS,
		Subject:       slack.Subject(envelope.Event.Text),
		ActorID:       conversation.Bounded(envelope.Event.User, conversation.MaxActorIDLength),
		ActorDisplay:  conversation.Bounded(envelope.Event.User, conversation.MaxActorDisplayLength),
		Text:          conversation.Bounded(envelope.Event.Text, conversation.MaxMessageTextLength),
	})
	if err != nil {
		h.Logger.ErrorContext(ctx, "recording a slack message failed",
			slog.String("request_id", requestID),
			slog.String("org_id", organization.String()),
			slog.String("integration_id", integration.String()),
			slog.String("error", err.Error()))
		h.counters.countRequest(ctx, surfaceSlack, resultError)
		writeStatus(writer, http.StatusServiceUnavailable, "not recorded")
		return
	}
	if outcome.Duplicate {
		h.counters.countRequest(ctx, surfaceSlack, resultDuplicate)
		writeStatus(writer, http.StatusOK, "already accepted")
		return
	}

	h.counters.countRequest(ctx, surfaceSlack, resultAccepted)
	h.Logger.InfoContext(ctx, "slack message accepted",
		slog.String("request_id", requestID),
		slog.String("org_id", organization.String()),
		slog.String("integration_id", integration.String()),
		slog.String("conversation_id", outcome.Conversation.String()),
		slog.Bool("opened_conversation", outcome.Opened))
	writeStatus(writer, http.StatusAccepted, "accepted")
}
