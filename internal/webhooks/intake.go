package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/correlation"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

const TokenHeader = integrations.WebhookTokenHeader

const AlertEventsPath = "/webhooks/v1/integrations/{integration}/alert-events"

const maxBodyBytes = 1 << 20

const readTimeout = 15 * time.Second

type Handlers struct {
	Database       *storage.Database
	Logger         *slog.Logger
	AlertAdmission storage.AlertAdmissionPolicy
	Adapters       Adapters
	Slack          *SlackAgent
}

type receiver struct {
	Handlers
	requests *limiter
	counters instruments
}

func newReceiver(handlers Handlers) *receiver {
	return &receiver{
		Handlers: handlers,
		requests: newLimiter(time.Now),
		counters: newInstruments(handlers.Logger),
	}
}

func (h Handlers) Router() http.Handler {
	receiver := newReceiver(h)

	mux := http.NewServeMux()
	mux.HandleFunc(alertEventsRoute, receiver.handleAlertEvents)

	// slack event ------->
	if h.Slack != nil && h.Slack.Serves() {
		mux.HandleFunc(slackEventsRoute, receiver.handleSlackEvents)
	}
	return receiver.limit(mux)
}

const alertEventsRoute = "POST " + AlertEventsPath

func (h *receiver) limit(next *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !h.requests.allowRequest() {
			writer.Header().Set("Retry-After", "1")
			status := http.StatusTooManyRequests
			surface := ""
			_, pattern := next.Handler(request)
			switch pattern {
			case alertEventsRoute:
				status = http.StatusServiceUnavailable
				surface = surfaceAlert
			case slackEventsRoute:
				surface = surfaceSlack
			}
			if surface != "" {
				h.counters.countRequest(request.Context(), surface, resultRateLimited)
				h.Logger.WarnContext(request.Context(), "webhook request rate limited",
					slog.String("request_id", correlation.From(request.Context())),
					slog.String("surface", surface))
			} else {
				h.Logger.WarnContext(request.Context(), "unmatched webhook request rate limited",
					slog.String("request_id", correlation.From(request.Context())))
			}
			writeStatus(writer, status, "slow down")
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func (h *receiver) handleAlertEvents(writer http.ResponseWriter, request *http.Request) {
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()
	requestID := correlation.From(ctx)

	integrationID, ok := h.integrationID(writer, request)
	if !ok {
		h.counters.countRequest(ctx, surfaceAlert, resultRejected)
		return
	}
	integration, adapter, err := h.authenticate(ctx, integrationID, request)
	if err != nil {
		// Authentication failures are permanent, but database failures must remain retryable;
		// returning 401 for an outage would make alert sources discard the delivery.
		if !errors.Is(err, errNotAuthenticated) {
			h.Logger.ErrorContext(ctx, "could not read the integration",
				slog.String("request_id", requestID),
				slog.String("caller", callerOf(request)),
				slog.String("error", err.Error()))
			h.counters.countRequest(ctx, surfaceAlert, resultError)
			writeStatus(writer, http.StatusServiceUnavailable, "not recorded")
			return
		}
		h.refuse(ctx, request, "unauthenticated")
		h.counters.countRequest(ctx, surfaceAlert, resultRejected)
		writeStatus(writer, http.StatusUnauthorized, "unauthorized")
		return
	}

	organization, err := uuid.Parse(strings.TrimSpace(integration.OrgID))
	if err != nil || organization == uuid.Nil {
		if err == nil {
			err = errors.New("invalid organization identifier")
		}
		h.Logger.ErrorContext(ctx, "an integration names an organization that is not a name",
			slog.String("request_id", requestID),
			slog.String("integration_id", integration.ID.String()),
			slog.String("error", err.Error()))
		h.counters.countRequest(ctx, surfaceAlert, resultError)
		writeStatus(writer, http.StatusServiceUnavailable, "not recorded")
		return
	}

	body, err := readBody(writer, request)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.refuse(ctx, request, "oversized")
			h.counters.countRequest(ctx, surfaceAlert, resultRejected)
			writeStatus(writer, http.StatusRequestEntityTooLarge, "payload too large")
			return
		}
		h.refuse(ctx, request, "incomplete")
		h.counters.countRequest(ctx, surfaceAlert, resultRejected)
		writeStatus(writer, http.StatusBadRequest, "payload not received")
		return
	}

	normalized, err := adapter.Normalize(body)
	if err != nil {
		h.refuse(ctx, request, "malformed")
		h.counters.countRequest(ctx, surfaceAlert, resultRejected)
		writeStatus(writer, http.StatusBadRequest, "payload not understood")
		return
	}

	h.recordAlertDelivery(ctx, writer, organization, requestID, storage.Delivery{
		Integration:   integration.ID,
		AlertDelivery: normalized,
	})
}

func (h *receiver) recordAlertDelivery(
	ctx context.Context, writer http.ResponseWriter,
	organization uuid.UUID, requestID string, delivery storage.Delivery,
) {
	outcome, err := h.Database.RecordDelivery(ctx, organization, delivery, h.AlertAdmission)
	var full storage.AlertCapacityError
	if errors.As(err, &full) {
		h.counters.countRequest(ctx, surfaceAlert, resultRateLimited)
		writer.Header().Set("Retry-After", "1")
		writeStatus(writer, http.StatusServiceUnavailable, "pending Investigation capacity exhausted")
		return
	}
	var tooLarge storage.AlertBatchTooLargeError
	if errors.As(err, &tooLarge) {
		h.counters.countRequest(ctx, surfaceAlert, resultRejected)
		writeStatus(writer, http.StatusBadRequest, "alert batch exceeds pending Investigation limit")
		return
	}
	if errors.Is(err, storage.ErrDeliveryIdentityConflict) {
		h.counters.countRequest(ctx, surfaceAlert, resultRejected)
		h.Logger.WarnContext(ctx, "delivery refused",
			slog.String("request_id", requestID),
			slog.String("org_id", organization.String()),
			slog.String("integration_id", delivery.Integration.String()),
			slog.String("reason", "identity conflict"))
		writeStatus(writer, http.StatusBadRequest, "event identity conflicts with accepted content")
		return
	}
	if err != nil {
		h.Logger.ErrorContext(ctx, "recording a delivery failed",
			slog.String("request_id", requestID),
			slog.String("org_id", organization.String()),
			slog.String("integration_id", delivery.Integration.String()),
			slog.String("error", err.Error()))
		h.counters.countRequest(ctx, surfaceAlert, resultError)
		writeStatus(writer, http.StatusServiceUnavailable, "not recorded")
		return
	}

	if outcome.Duplicate {
		h.counters.countRequest(ctx, surfaceAlert, resultDuplicate)
		h.Logger.InfoContext(ctx, "delivery already accepted",
			slog.String("request_id", requestID),
			slog.String("org_id", organization.String()),
			slog.String("integration_id", delivery.Integration.String()))
		writeStatus(writer, http.StatusOK, "already accepted")
		return
	}

	if delivery.Truncated > 0 {
		h.Logger.WarnContext(ctx, "the source truncated this delivery",
			slog.String("request_id", requestID),
			slog.String("org_id", organization.String()),
			slog.String("integration_id", delivery.Integration.String()),
			slog.Int("omitted", delivery.Truncated))
	}

	h.counters.countRequest(ctx, surfaceAlert, resultAccepted)
	h.counters.countAlertEvents(ctx, outcome.Recorded)
	h.Logger.InfoContext(ctx, "delivery accepted",
		slog.String("request_id", requestID),
		slog.String("org_id", organization.String()),
		slog.String("integration_id", delivery.Integration.String()),
		slog.Int("alertEvents", outcome.Recorded),
		slog.Int("episodes_opened", outcome.IncidentsOpened),
		slog.Int("episodes_joined", outcome.IncidentsJoined))
	writeStatus(writer, http.StatusAccepted, "accepted")
}

var errNotAuthenticated = errors.New("not authenticated")

func (h *receiver) authenticate(
	ctx context.Context, integrationID uuid.UUID, request *http.Request,
) (integrations.Integration, Adapter, error) {

	integration, err := h.Database.IntegrationByID(ctx, integrationID)
	switch {
	case errors.Is(err, integrations.ErrUnknown):
		return integrations.Integration{}, nil, fmt.Errorf("%w: no such integration", errNotAuthenticated)
	case err != nil:
		return integrations.Integration{}, nil, err
	}

	adapter, served := h.Adapters[integration.Provider]
	if !served {
		if !integrations.AuthenticateWebhookToken(request.Header, integration) {
			return integration, nil, fmt.Errorf("%w: credential does not match", errNotAuthenticated)
		}
		return integration, nil, fmt.Errorf("integration provider %q is not served", integration.Provider)
	}
	if !adapter.Authenticate(request.Header, integration) {
		return integration, adapter, fmt.Errorf("%w: credential does not match", errNotAuthenticated)
	}
	if integration.Disabled {
		return integration, adapter, fmt.Errorf("%w: integration is disabled", errNotAuthenticated)
	}
	return integration, adapter, nil
}

func callerOf(request *http.Request) string {
	return request.RemoteAddr
}

func (h *receiver) integrationID(
	writer http.ResponseWriter, request *http.Request,
) (uuid.UUID, bool) {
	integrationID, err := uuid.Parse(request.PathValue("integration"))
	if err != nil {
		writeStatus(writer, http.StatusUnauthorized, "unauthorized")
		return uuid.UUID{}, false
	}
	return integrationID, true
}

func readBody(writer http.ResponseWriter, request *http.Request) ([]byte, error) {
	limited := http.MaxBytesReader(writer, request.Body, maxBodyBytes)
	return io.ReadAll(limited)
}

func (h *receiver) refuse(ctx context.Context, request *http.Request, reason string) {
	// Never log the body or headers: both are untrusted and headers may contain credential guesses.
	h.Logger.WarnContext(ctx, "delivery refused",
		slog.String("request_id", correlation.From(ctx)),
		slog.String("integration_id", request.PathValue("integration")),
		slog.String("caller", callerOf(request)),
		slog.String("reason", reason))
}

type statusBody struct {
	Status string `json:"status"`
}

func writeStatus(writer http.ResponseWriter, code int, status string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(code)
	_ = json.NewEncoder(writer).Encode(statusBody{Status: status})
}
