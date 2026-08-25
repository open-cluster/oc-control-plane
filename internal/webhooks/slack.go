package webhooks

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/conversation"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/integrations/slack"
	"github.com/open-cluster/oc-control-plane/internal/storage"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// SLACK EVENTS LIVE ON INTAKE, NOT ON THE OPERATOR API.
//
// Intake is already the surface a customer's own infrastructure reaches inbound. It already
// bounds bodies as they are read, already rate-limits per source, already refuses without
// saying which half of a guess was right, and already owns the Delivery record this reuses
// for deduplication. The operator API is the opposite of all of that: it reads across a
// tenant's records for an authenticated person and belongs on an interface a deployment can
// keep private.
//
// ONE FIXED PATH. A Slack app registration holds a single request URL, so there is no
// identifier in this path and there could not be: the workspace is read from the signed body
// and resolved through the installation record, which is what makes the tenant DISCOVERED
// rather than claimed.
//
// THE ORDER IS THE POINT, AND EVERY STEP IS A REFUSAL:
//
//	read the unmodified raw body, under the existing bound
//	verify the signature over the versioned base string, in constant time
//	reject a timestamp outside the replay window
//	answer the URL verification challenge, creating nothing
//	resolve the installation to an integration and therefore an organization
//	discard what is not a person speaking to us — our own messages first
//	deduplicate, persist, acknowledge
//
// The handler never waits on a model, a repository read, a cluster read or an investigation.
// It persists and returns. The turn is claimed asynchronously by the same worker that runs
// every other turn, which is what keeps acknowledgement well inside Slack's timeout however
// long the investigation behind it takes.

// SlackEventsPath is where a workspace delivers. It is a constant because it goes into a
// Slack app registration, which a customer or an operator configures once and then does not
// touch — a path that moved would be an app registration to edit in somebody else's system.
const SlackEventsPath = "/intake/v1/slack/events"

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
	Enabled func(tenancy.Organization) bool
	// WindowLead is how far before an incident a turn's window reaches back, passed
	// through to the turn exactly as the console path passes it.
	WindowLead time.Duration
	// MaxWaitingTurns bounds one organization's unclaimed turns, so overload is a plain
	// refusal rather than a queue that grows without bound.
	MaxWaitingTurns int
}

// Serves reports whether this deployment receives Slack events at all.
func (s *SlackAgent) Serves() bool { return s != nil && s.SigningSecret != "" }

// slackEvents receives one events request.
func (h *surface) slackEvents(writer http.ResponseWriter, request *http.Request) {
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	// Acknowledgement latency is MEASURED rather than reasoned about. Slack retries
	// anything it is not answered inside three seconds, so a deployment drifting towards
	// that ceiling is a retry storm this product would be asking for — and nobody would
	// see it coming from a counter of outcomes.
	began := time.Now()
	defer func() { h.counters.observeSlackLatency(ctx, time.Since(began)) }()

	// The raw body, exactly as received and under the same bound every other delivery is
	// read under. It must not be re-encoded before the signature is checked: a body round
	// -tripped through a decoder is a different body, and verifying that one would be
	// verifying something the far end never signed.
	body, err := readBody(writer, request)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeStatus(writer, http.StatusRequestEntityTooLarge, "payload too large")
			return
		}
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
		h.counters.countSlackEvent(ctx, slackRefused)
		writeStatus(writer, http.StatusUnauthorized, "unauthorized")
		return
	}

	envelope, err := slack.Parse(body)
	if err != nil {
		h.counters.countSlackEvent(ctx, slackMalformed)
		writeStatus(writer, http.StatusBadRequest, "payload not understood")
		return
	}

	// URL verification. Answered before anything is resolved, because it is how a workspace
	// proves this endpoint is ours BEFORE any installation exists — and it creates nothing.
	if envelope.Challenge != "" {
		h.counters.countSlackEvent(ctx, slackChallenge)
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Cache-Control", "no-store")
		writer.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(writer).Encode(map[string]string{"challenge": envelope.Challenge})
		return
	}

	integration, routing, err := h.Database.IntegrationByInstallation(ctx,
		integrations.TypeSlack, integrations.InstallationKey(envelope.Key()))
	if err != nil {
		if errors.Is(err, integrations.ErrUnknown) {
			// A workspace this deployment does not know. Refused WITHOUT saying so: the
			// answer is the same one a bad signature gets, because telling a caller that
			// the signature was fine and the workspace unknown tells them a valid signing
			// secret when they have one.
			h.counters.countSlackEvent(ctx, slackUnknownWorkspace)
			writeStatus(writer, http.StatusUnauthorized, "unauthorized")
			return
		}
		h.Logger.ErrorContext(ctx, "could not resolve a slack installation",
			slog.String("error", err.Error()))
		h.counters.countSlackEvent(ctx, slackUnavailable)
		writeStatus(writer, http.StatusServiceUnavailable, "not recorded")
		return
	}

	organization, err := tenancy.NewOrganization(integration.OrgID)
	if err != nil {
		h.Logger.ErrorContext(ctx, "a slack installation names an organization that is not a name",
			slog.String("integration_id", integration.ID.String()),
			slog.String("error", err.Error()))
		h.counters.countSlackEvent(ctx, slackUnavailable)
		writeStatus(writer, http.StatusServiceUnavailable, "not recorded")
		return
	}

	// The same per-source limit every other inbound delivery is held to, applied once the
	// SOURCE is known. It is after resolution rather than before because the work worth
	// bounding is the writes, and because there is no source to attribute a request to
	// until its signature has been checked against an installation this deployment knows.
	if !h.deliveries.allow(integration.ID) {
		// Shed rather than refused. Slack is told to slow down, not to stop, because the
		// questions behind this one are real and it should still send them. Nothing is
		// written to the delivery history, exactly as the alert path writes nothing: a
		// limiter that cost a row per shed request would be a limiter that amplifies.
		h.counters.countSlackEvent(ctx, slackRateLimited)
		writer.Header().Set("Retry-After", "1")
		writeStatus(writer, http.StatusTooManyRequests, "slow down")
		return
	}

	// Everything from here is ACKNOWLEDGED whatever happens to it. Slack retries anything it
	// is not told succeeded, and retrying an event this build deliberately ignores would be
	// a storm this deployment asked for.
	switch {
	case integration.Disabled():
		// An operator turned this integration off. Reading stops and answering stops, and
		// the event is dropped rather than queued: a switch that quietly accumulated work
		// to do later is not a switch.
		h.counters.countSlackEvent(ctx, slackDisabled)
		writeStatus(writer, http.StatusOK, "ignored")
		return
	case h.Slack.Enabled == nil || !h.Slack.Enabled(organization):
		// Outside the staged rollout. Acknowledged and dropped, and reads through the
		// existing Slack tools are untouched by this.
		h.counters.countSlackEvent(ctx, slackOutsideRollout)
		writeStatus(writer, http.StatusOK, "ignored")
		return
	case !envelope.AddressedToUs(routing.Agent):
		// Not a person speaking to us: a channel join, an edit, a reaction, another app
		// posting, or OUR OWN message — which is checked first inside, because an agent
		// that answers its own message answers its answer until a rate limit ends it.
		h.counters.countSlackEvent(ctx, slackNotAddressed)
		writeStatus(writer, http.StatusOK, "ignored")
		return
	}

	h.acceptSlackMessage(ctx, writer, organization, integration.ID, body, envelope)
}

// acceptSlackMessage persists the message and durable work in one transaction, then answers.
func (h *surface) acceptSlackMessage(
	ctx context.Context, writer http.ResponseWriter, organization tenancy.Organization,
	integration uuid.UUID, body []byte, envelope slack.Envelope,
) {
	digest := sha256.Sum256(body)
	outcome, err := h.Database.RecordSlackMessage(ctx, organization, storage.SlackMessage{
		Integration: integration,
		BodyDigest:  digest[:],
		Channel:     envelope.Event.Channel,
		Thread:      envelope.Thread(),
		MessageID:   envelope.Event.TS,
		Subject:     slack.Subject(envelope.Event.Text),
		ActorID:     conversation.Bounded(envelope.Event.User, conversation.MaxActorIDLength),
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
		h.counters.countSlackEvent(ctx, slackUnavailable)
		writeStatus(writer, http.StatusServiceUnavailable, "not recorded")
		return
	}
	if outcome.Duplicate {
		// The same body already accepted through this integration. That covers a workspace
		// retrying because it never saw our answer — which has done nothing wrong and whose
		// answer must let it stop — and a body replayed by somebody who captured it, which
		// is applied to nothing for the same reason.
		//
		// Recorded in the delivery history, so a source that is delivering and being turned
		// away stays distinguishable from one that has gone quiet.
		h.recordAttempt(ctx, organization, integration, storage.DeliveryDuplicate)
		h.counters.countSlackEvent(ctx, slackDuplicate)
		writeStatus(writer, http.StatusOK, "already accepted")
		return
	}

	h.counters.countSlackEvent(ctx, slackAccepted)
	h.counters.work.Count(ctx, "accepted")
	h.Logger.InfoContext(ctx, "slack message accepted",
		slog.String("org_id", organization.String()),
		slog.String("integration_id", integration.String()),
		slog.String("conversation_id", outcome.Conversation.String()),
		slog.Bool("opened_conversation", outcome.Opened))
	writeStatus(writer, http.StatusOK, "accepted")
}
