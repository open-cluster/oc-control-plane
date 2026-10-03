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

// SlackEventsPath is where a workspace delivers. It is a constant because it goes into a
// Slack app registration, which a customer or an operator configures once and then does not
// touch — a path that moved would be an app registration to edit in somebody else's system.
const SlackEventsPath = "/webhooks/v1/slack/events"

// SlackAgent is what this listener needs to serve Slack events, and is nil where a deployment
// serves none.
//
// A deployment with no signing secret does not serve the endpoint AT ALL, rather than serving
// one that refuses everything. The two look the same to an attacker and are very different to
// an operator: an endpoint that exists and refuses is a configuration to check, and an
// endpoint that does not exist is a deployment that was never asked to receive events.
type SlackAgent struct {
	// SigningSecret is what inbound requests are verified against. Empty is not a valid
	// SlackAgent; the composition root builds none.
	SigningSecret string
	// Enabled reports whether the Slack agent surface is live for one organization.
	//
	// It is a STAGED ROLLOUT gate and nothing else: a deployment-level list, off when
	// unset, so this can be dogfooded in one workspace and enabled for a design partner
	// without a customer-facing setting that would outlive the rollout. It is not tenant
	// policy — an organization that has not connected Slack does not have Slack, and an
	// integration already carries a per-integration switch an operator can use.
	//
	// Nil means no organization is inside the gate.
	Enabled func(uuid.UUID) bool
	// WindowLead is how far before an incident a turn's window reaches back, passed
	// through to the turn exactly as the console path passes it.
	WindowLead time.Duration
	// MaxWaitingTurns bounds one organization's unclaimed turns, so overload is a plain
	// refusal rather than a queue that grows without bound.
	MaxWaitingTurns int
}

// Serves reports whether this deployment receives Slack events at all.
func (s *SlackAgent) Serves() bool { return s != nil && s.SigningSecret != "" }

// handleSlackEvents receives one events request.
func (h *receiver) handleSlackEvents(writer http.ResponseWriter, request *http.Request) {
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()
	requestID := correlation.From(ctx)

	// Acknowledgement latency is MEASURED rather than reasoned about. Slack retries
	// anything it is not answered inside three seconds, so a deployment drifting towards
	// that ceiling is a retry storm this product would be asking for — and nobody would
	// see it coming from a counter of outcomes.
	began := time.Now()
	defer func() { h.counters.observeSlackAcknowledgement(ctx, time.Since(began)) }()

	// The raw body, exactly as received and under the same bound every other delivery is
	// read under. It must not be re-encoded before the signature is checked: a body round
	// -tripped through a decoder is a different body, and verifying that one would be
	// verifying something the far end never signed.
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
		// Logged apart, answered the same. An operator debugging a broken registration
		// needs to know a signature failed rather than a timestamp; a caller learning which
		// is a caller learning which half of a guess was right.
		h.Logger.WarnContext(ctx, "a slack events request was refused",
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

	// URL verification. Answered before anything is resolved, because it is how a workspace
	// proves this endpoint is ours BEFORE any installation exists — and it creates nothing.
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
			// A workspace this deployment does not know. Refused WITHOUT saying so: the
			// answer is the same one a bad signature gets, because telling a caller that
			// the signature was fine and the workspace unknown tells them a valid signing
			// secret when they have one.
			h.counters.countRequest(ctx, surfaceSlack, resultRejected)
			writeStatus(writer, http.StatusUnauthorized, "unauthorized")
			return
		}
		h.Logger.ErrorContext(ctx, "could not resolve a slack installation",
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
			slog.String("integration_id", integration.ID.String()),
			slog.String("error", err.Error()))
		h.counters.countRequest(ctx, surfaceSlack, resultError)
		writeStatus(writer, http.StatusServiceUnavailable, "not recorded")
		return
	}

	// The same per-source limit every other inbound delivery is held to, applied once the
	// SOURCE is known. It is after resolution rather than before because the work worth
	// bounding is the writes, and because there is no source to attribute a request to
	// until its signature has been checked against an installation this deployment knows.
	// Everything from here is ACKNOWLEDGED whatever happens to it. Slack retries anything it
	// is not told succeeded, and retrying an event this build deliberately ignores would be
	// a storm this deployment asked for.
	switch {
	case integration.Disabled:
		// An operator turned this integration off. Reading stops and answering stops, and
		// the event is dropped rather than queued: a switch that quietly accumulated work
		// to do later is not a switch.
		h.counters.countRequest(ctx, surfaceSlack, resultAccepted)
		writeStatus(writer, http.StatusOK, "ignored")
		return
	case h.Slack.Enabled == nil || !h.Slack.Enabled(organization):
		// Outside the staged rollout. Acknowledged and dropped, and reads through the
		// existing Slack tools are untouched by this.
		h.counters.countRequest(ctx, surfaceSlack, resultAccepted)
		writeStatus(writer, http.StatusOK, "ignored")
		return
	case !envelope.AddressedToUs(routing.ProviderActorID):
		// Not a person speaking to us: a channel join, an edit, a reaction, another app
		// posting, or OUR OWN message — which is checked first inside, because an agent
		// that answers its own message answers its answer until a rate limit ends it.
		h.counters.countRequest(ctx, surfaceSlack, resultAccepted)
		writeStatus(writer, http.StatusOK, "ignored")
		return
	}

	h.acceptSlackMessage(ctx, writer, organization, integration.ID, requestID, body, envelope)
}

// acceptSlackMessage persists the message and durable work in one transaction, then answers.
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
		// The display name is the Slack user id until a name is resolved. Resolving it
		// needs a users.info read this handler must not make: it would put a vendor call
		// on the acknowledgement path, which is the one thing this endpoint may not do.
		ActorDisplay: conversation.Bounded(envelope.Event.User, conversation.MaxActorDisplayLength),
		Text:         conversation.Bounded(envelope.Event.Text, conversation.MaxMessageTextLength),
	})
	if err != nil {
		h.Logger.ErrorContext(ctx, "recording a slack message failed",
			slog.String("org_id", organization.String()),
			slog.String("integration_id", integration.String()),
			slog.String("error", err.Error()))
		h.counters.countRequest(ctx, surfaceSlack, resultError)
		writeStatus(writer, http.StatusServiceUnavailable, "not recorded")
		return
	}
	if outcome.Duplicate {
		// The same body already accepted through this integration. That covers a workspace
		// retrying because it never saw our answer — which has done nothing wrong and whose
		// answer must let it stop — and a body replayed by somebody who captured it, which
		// is applied to nothing for the same reason.
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
