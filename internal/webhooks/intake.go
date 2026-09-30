// Package webhooks accepts alerts from the systems a customer already runs.
//
// Intake owns its authenticated, bounded route tree. The application mounts that tree on the
// shared HTTP listener; a reverse proxy may apply path-specific exposure without bypassing
// provider authentication or body limits.
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

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

// TokenHeader authenticates the sender but does not attest the body. Adapters that support
// signatures verify them directly.
const TokenHeader = integrations.WebhookTokenHeader

const AlertEventsPath = "/webhooks/v1/integrations/{integration}/alert-events"

// maxBodyBytes bounds a delivery. It is enforced as the body is read rather than after, so an
// oversized payload is refused without ever being held whole — intake is reachable by anything
// that can guess an Integration identifier, and a size bound applied after buffering is not a
// bound.
const maxBodyBytes = 1 << 20

// readTimeout bounds how long one delivery may take. A source that opens a connection and
// then goes quiet must not be able to hold it.
const readTimeout = 15 * time.Second

// Handlers is the intake surface's dependencies.
type Handlers struct {
	Database       *storage.Database
	Logger         *slog.Logger
	AlertAdmission storage.AlertAdmissionPolicy
	// Adapters routes a payload to its type's parser. Supplied by the composition root,
	// which is the only place that knows every provider.
	Adapters Adapters
	// Slack is what this listener needs to receive Slack events, and nil where the
	// deployment holds no signing secret. A deployment with none does not serve the
	// endpoint at all rather than serving one that refuses everything: an endpoint that
	// exists and refuses is a configuration to check, and one that does not exist is a
	// deployment nobody asked to receive events.
	Slack *SlackAgent
}

// receiver is one running intake listener: its dependencies plus the state that belongs to a
// listener rather than to a configuration. The rate limiter is per receiver because it holds
// live counters, and a Handlers value that carried them could be copied into two limiters
// enforcing half a limit each.
type receiver struct {
	Handlers
	deliveries *limiter
	// counters are this listener's own instruments. They are per receiver for the same reason the
	// limiter is: an instrument rebuilt per request is a new time series per request.
	counters instruments
}

func newReceiver(handlers Handlers) *receiver {
	return &receiver{
		Handlers:   handlers,
		deliveries: newLimiter(time.Now),
		counters:   newInstruments(handlers.Logger),
	}
}

// Router returns the intake surface.
//
// The route names the Integration and nothing else. There is no organization in it, and
// adding one would be adding a tenant identifier the caller chooses.
func (h Handlers) Router() http.Handler {
	receiver := newReceiver(h)

	mux := http.NewServeMux()
	mux.HandleFunc(alertEventsRoute, receiver.handleAlertEvents)
	if h.Slack != nil && h.Slack.Serves() {
		mux.HandleFunc("POST "+SlackEventsPath, receiver.handleSlackEvents)
	}
	return receiver.limit(mux)
}

const alertEventsRoute = "POST " + AlertEventsPath

func (h *receiver) limit(next *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !h.deliveries.allowRequest() {
			h.counters.countDelivery(request.Context(), dispositionRateLimited)
			writer.Header().Set("X-Request-ID", uuid.NewString())
			writer.Header().Set("Retry-After", "1")
			status := http.StatusTooManyRequests
			if _, pattern := next.Handler(request); pattern == alertEventsRoute {
				status = http.StatusServiceUnavailable
			}
			writeStatus(writer, status, "slow down")
			return
		}
		next.ServeHTTP(writer, request)
	})
}

// handleAlertEvents accepts one webhook delivery.
//
// Integration quota is spent only after authentication, before payload normalization.
func (h *receiver) handleAlertEvents(writer http.ResponseWriter, request *http.Request) {
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()
	requestID := uuid.NewString()
	writer.Header().Set("X-Request-ID", requestID)

	integrationID, ok := h.integrationID(writer, request)
	if !ok {
		return
	}
	integration, adapter, err := h.authenticate(ctx, integrationID, request)
	if err != nil {
		// A failure to READ the Integration is not a failure to authenticate, and answering
		// it as one would be the worst mistake available here: 401 is permanent, so a
		// database outage would tell every source to give up, and the alerts they would
		// otherwise have retried are gone for good. Only an Integration that was read and
		// did not match is refused.
		if !errors.Is(err, errNotAuthenticated) {
			h.Logger.ErrorContext(ctx, "could not read the integration",
				slog.String("caller", callerOf(request)),
				slog.String("error", err.Error()))
			h.counters.countDelivery(ctx, dispositionUnavailable)
			writeStatus(writer, http.StatusServiceUnavailable, "not recorded")
			return
		}
		h.refuse(ctx, request, "unauthenticated")
		h.counters.countDelivery(ctx, dispositionUnauthenticated)
		// One status and one message however it failed. A missing header, a wrong secret,
		// an unknown Integration, a disabled one and one that receives no webhooks are
		// indistinguishable, because telling them apart is how a caller learns which half
		// of a guess was right.
		writeStatus(writer, http.StatusUnauthorized, "unauthorized")
		return
	}

	if !h.deliveries.allow(integration.ID) {
		h.refuse(ctx, request, "rate limited")
		h.counters.countDelivery(ctx, dispositionRateLimited)
		writer.Header().Set("Retry-After", "1")
		writeStatus(writer, http.StatusServiceUnavailable, "slow down")
		return
	}

	// The tenant is now known, and it was DISCOVERED rather than claimed: it comes from the
	// row whose secret just matched. Nothing the caller sent contributed to it.
	organization, err := uuid.Parse(strings.TrimSpace(integration.OrgID))
	if err != nil || organization == uuid.Nil {
		if err == nil {
			err = errors.New("invalid organization identifier")
		}
		h.Logger.ErrorContext(ctx, "an integration names an organization that is not a name",
			slog.String("integration_id", integration.ID.String()),
			slog.String("error", err.Error()))
		h.counters.countDelivery(ctx, dispositionUnavailable)
		writeStatus(writer, http.StatusServiceUnavailable, "not recorded")
		return
	}

	body, err := readBody(writer, request)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.refuse(ctx, request, "oversized")
			h.counters.countDelivery(ctx, dispositionOversized)
			writeStatus(writer, http.StatusRequestEntityTooLarge, "payload too large")
			return
		}
		h.refuse(ctx, request, "incomplete")
		h.counters.countDelivery(ctx, dispositionIncomplete)
		writeStatus(writer, http.StatusBadRequest, "payload not received")
		return
	}

	normalized, err := adapter.Normalize(body)
	if err != nil {
		// The payload is not what this type's adapter accepts. Retrying will not change
		// that, so the status has to say permanent or the source will retry a storm of them.
		h.refuse(ctx, request, "malformed")
		h.counters.countDelivery(ctx, dispositionMalformed)
		writeStatus(writer, http.StatusBadRequest, "payload not understood")
		return
	}

	h.recordAlertDelivery(ctx, writer, organization, storage.Delivery{
		Integration:   integration.ID,
		RequestID:     requestID,
		AlertDelivery: normalized,
	})
}

// recordAlertDelivery commits the delivery and answers the source.
func (h *receiver) recordAlertDelivery(
	ctx context.Context, writer http.ResponseWriter,
	organization uuid.UUID, delivery storage.Delivery,
) {
	outcome, err := h.Database.RecordDelivery(ctx, organization, delivery, h.AlertAdmission)
	var full storage.AlertCapacityError
	if errors.As(err, &full) {
		h.counters.countDelivery(ctx, dispositionUnavailable)
		writer.Header().Set("Retry-After", "1")
		writeStatus(writer, http.StatusServiceUnavailable, "pending Investigation capacity exhausted")
		return
	}
	var tooLarge storage.AlertBatchTooLargeError
	if errors.As(err, &tooLarge) {
		h.counters.countDelivery(ctx, dispositionOversized)
		writeStatus(writer, http.StatusBadRequest, "alert batch exceeds pending Investigation limit")
		return
	}
	if errors.Is(err, storage.ErrDeliveryIdentityConflict) {
		h.counters.countDelivery(ctx, dispositionMalformed)
		h.Logger.WarnContext(ctx, "delivery refused",
			slog.String("org_id", organization.String()),
			slog.String("integration_id", delivery.Integration.String()),
			slog.String("reason", "identity conflict"))
		writeStatus(writer, http.StatusBadRequest, "event identity conflicts with accepted content")
		return
	}
	if err != nil {
		// Nothing was written, and the source should try again — this is the one failure that
		// is genuinely ours and genuinely transient.
		h.Logger.ErrorContext(ctx, "recording a delivery failed",
			slog.String("org_id", organization.String()),
			slog.String("integration_id", delivery.Integration.String()),
			slog.String("error", err.Error()))
		h.counters.countDelivery(ctx, dispositionUnavailable)
		writeStatus(writer, http.StatusServiceUnavailable, "not recorded")
		return
	}

	if outcome.Duplicate {
		h.counters.countDelivery(ctx, dispositionDuplicate)
		// This body was already accepted through this Integration. That covers both a source
		// retrying because it never saw a response — which has done nothing wrong, and whose
		// answer must let it stop — and a body replayed by someone who captured it, which is
		// applied to nothing for the same reason.
		h.Logger.InfoContext(ctx, "delivery already accepted",
			slog.String("org_id", organization.String()),
			slog.String("integration_id", delivery.Integration.String()))
		writeStatus(writer, http.StatusOK, "already accepted")
		return
	}

	// A truncated delivery is recorded and reported, not refused. The alerts that did arrive
	// are real and refusing would lose them, since the source will not send them again — but a
	// truncation is the sender saying this platform's record of the moment is incomplete, and
	// that must be visible rather than inferred from a count that looks fine.
	if delivery.Truncated > 0 {
		h.Logger.WarnContext(ctx, "the source truncated this delivery",
			slog.String("org_id", organization.String()),
			slog.String("integration_id", delivery.Integration.String()),
			slog.Int("omitted", delivery.Truncated))
	}

	h.counters.countDelivery(ctx, dispositionAccepted)
	h.counters.jobs.Count(ctx, "accepted")
	h.counters.countAlertEvents(ctx, outcome.Recorded, outcome.IncidentsOpened, outcome.IncidentsJoined)
	h.Logger.InfoContext(ctx, "delivery accepted",
		slog.String("org_id", organization.String()),
		slog.String("integration_id", delivery.Integration.String()),
		slog.Int("alertEvents", outcome.Recorded),
		slog.Int("episodes_opened", outcome.IncidentsOpened),
		slog.Int("episodes_joined", outcome.IncidentsJoined))
	writeStatus(writer, http.StatusAccepted, "accepted")
}

// errNotAuthenticated marks the failures that are the caller's: no credential, a wrong
// one, an Integration that does not exist, one that has been turned off, and one that
// receives no webhooks. Everything else reaching the caller of authenticate is ours, and
// must not be answered as a refusal.
var errNotAuthenticated = errors.New("not authenticated")

// authenticate resolves the Integration from its opaque identifier and checks the secret
// it was configured with.
//
// The identifier is looked up across the database this deployment serves, and the row
// that is found is the authority for the organization. The comparison is constant-time and
// happens whether or not a secret is held, so the answer says nothing about which
// identifiers exist.
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
		// An operator who turned an Integration off wants deliveries refused, not merely
		// recorded.
		return integration, adapter, fmt.Errorf("%w: integration is disabled", errNotAuthenticated)
	}
	return integration, adapter, nil
}

// callerOf reports where a delivery came from. It is what makes a campaign of credential
// guesses investigable: this surface has no operator identity behind it, so an address is the
// whole of the attribution available.
func callerOf(request *http.Request) string {
	return request.RemoteAddr
}

// integrationID resolves the Integration named in the path.
//
// A path that does not parse is answered exactly as a wrong secret is: same status, same body.
// Anything else lets a caller separate "this is not the shape of an identifier" from "this is
// not an integration", and probing the first is how you learn to probe the second.
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

// readBody reads the delivery under its bound. MaxBytesReader stops at the limit rather than
// after it, so an oversized payload is never held whole.
func readBody(writer http.ResponseWriter, request *http.Request) ([]byte, error) {
	limited := http.MaxBytesReader(writer, request.Body, maxBodyBytes)
	return io.ReadAll(limited)
}

// refuse records a rejected delivery: its reason, its Integration and where it came from, and
// never its payload. The body is untrusted text from a customer's systems, and a log that
// quoted it would turn diagnosis into a disclosure channel. The caller's address is recorded
// for the opposite reason — without it a campaign of credential guesses leaves nothing to
// investigate.
//
// Recording the addressed Integration rather than an Organization keeps refusals attributable
// before and after authentication without accepting a caller-supplied tenant claim.
//
// Nothing the caller sent in a header is recorded. A refused delivery's headers are the one
// place guaranteed to hold a guess at the credential.
func (h *receiver) refuse(ctx context.Context, request *http.Request, reason string) {
	h.Logger.WarnContext(ctx, "delivery refused",
		slog.String("integration_id", request.PathValue("integration")),
		slog.String("caller", callerOf(request)),
		slog.String("reason", reason))
}

// statusBody is what every answer this surface gives looks like.
type statusBody struct {
	Status string `json:"status"`
}

// writeStatus answers the source. An encoding failure cannot be reported — the status is
// already written — so it is dropped here and would surface as a truncated body, which is
// visibly wrong rather than quietly wrong.
//
// Nothing this surface returns may be cached: an answer concerns a named tenant's Integration,
// and an intermediary holding one response is a cross-tenant disclosure waiting for the next
// request.
func writeStatus(writer http.ResponseWriter, code int, status string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(code)
	_ = json.NewEncoder(writer).Encode(statusBody{Status: status})
}
