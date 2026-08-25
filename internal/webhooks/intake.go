// Package intake accepts alerts from the systems a customer already runs.
//
// The product does not detect. Building an alerting engine would rebuild what every customer
// already has and would contradict the position that this is an investigation platform rather
// than a monitoring one. What customers do not have is anything that starts investigating
// before a human arrives, and reaching that means accepting their alerts rather than replacing
// them.
//
// A delivery names its Integration and NOTHING else. The organization is read from the
// authenticated Integration row, because a path is chosen by the caller and a caller who
// could name a tenant could try every tenant.
//
// Accepting alerts is not a thin adapter. Each Integration Type has its own payload, its
// own idea of authentication, its own retry behaviour and its own notion of what counts as
// the same alert firing twice. Every one of those differences is confined to the type's
// provider package; past normalisation nothing can tell which system delivered a Signal.
//
// Intake owns its authenticated, bounded route tree. The application mounts that tree on the
// shared HTTP listener; a reverse proxy may apply path-specific exposure without bypassing
// provider authentication or body limits.
package webhooks

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/storage"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// TokenHeader carries the Integration's webhook secret.
//
// It is a header rather than a signature because the first inbound type cannot sign:
// Alertmanager attaches static headers to a webhook and nothing more. The consequence is
// written down rather than left implicit — this authenticates the sender and attests nothing
// about the body — and verification is per-adapter precisely so a type that can sign gets a
// signature instead.
const TokenHeader = "X-OpenCluster-Token"

// maxBodyBytes bounds a delivery. It is enforced as the body is read rather than after, so an
// oversized payload is refused without ever being held whole — intake is reachable by anything
// that can guess an Integration identifier, and a size bound applied after buffering is not a
// bound.
const maxBodyBytes = 1 << 20

// maxPresentedSecret bounds what may arrive in the token header, so a caller cannot make every
// refused delivery hash a megabyte.
const maxPresentedSecret = 256

// readTimeout bounds how long one delivery may take. A source that opens a connection and
// then goes quiet must not be able to hold it.
const readTimeout = 15 * time.Second

// Handlers is the intake surface's dependencies.
type Handlers struct {
	Database *storage.Database
	Logger   *slog.Logger
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

// surface is one running intake listener: its dependencies plus the state that belongs to a
// listener rather than to a configuration. The rate limiter is per surface because it holds
// live counters, and a Handlers value that carried them could be copied into two limiters
// enforcing half a limit each.
type surface struct {
	Handlers
	deliveries *limiter
	// counters are this listener's own instruments. They are per surface for the same reason the
	// limiter is: an instrument rebuilt per request is a new time series per request.
	counters instruments
}

// Router returns the intake surface.
//
// The route names the Integration and nothing else. There is no organization in it, and
// adding one would be adding a tenant identifier the caller chooses.
func (h Handlers) Router() http.Handler {
	running := &surface{
		Handlers:   h,
		deliveries: newLimiter(time.Now),
		counters:   newInstruments(h.Logger),
	}

	mux := http.NewServeMux()
	mux.Handle("POST /intake/v1/integrations/{integration}/signals",
		http.HandlerFunc(running.deliver))
	if h.Slack.Serves() {
		mux.Handle("POST "+SlackEventsPath, http.HandlerFunc(running.slackEvents))
	}
	return mux
}

// deliver accepts one webhook delivery.
//
// The order is the point. The rate limit comes first, because it is the only defence that must
// hold when everything after it is being abused. Authentication comes next, before the body is
// touched, so an unauthenticated caller reaches none of the parser — which is the part that
// handles input nobody has vouched for. Only then is the body read, under a bound, and only
// then parsed.
func (h *surface) deliver(writer http.ResponseWriter, request *http.Request) {
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	integrationID, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	if !h.deliveries.allow(integrationID) {
		// Shed rather than refused. The source is told to slow down, not to stop, because the
		// alerts behind this one are real and it should still send them.
		h.refuse(ctx, request, "rate limited")
		h.counters.countDelivery(ctx, dispositionRateLimited)
		writer.Header().Set("Retry-After", "1")
		writeStatus(writer, http.StatusTooManyRequests, "slow down")
		return
	}

	integration, err := h.authenticate(ctx, integrationID, request)
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
		// Recorded against the Integration when there IS one — which is what makes a source
		// delivering with a wrong secret visible as a rejection rather than as silence. When
		// the identifier resolved to nothing there is no tenant to attribute it to and
		// nothing is written, which is also what bounds this: an attacker with random
		// identifiers writes no rows.
		h.recordRefusal(ctx, integration, storage.RefusedUnauthenticated)
		h.refuse(ctx, request, "unauthenticated")
		h.counters.countDelivery(ctx, dispositionUnauthenticated)
		// One status and one message however it failed. A missing header, a wrong secret,
		// an unknown Integration, a disabled one and one that receives no webhooks are
		// indistinguishable, because telling them apart is how a caller learns which half
		// of a guess was right.
		writeStatus(writer, http.StatusUnauthorized, "unauthorized")
		return
	}

	// The tenant is now known, and it was DISCOVERED rather than claimed: it comes from the
	// row whose secret just matched. Nothing the caller sent contributed to it.
	organization, err := tenancy.NewOrganization(integration.OrgID)
	if err != nil {
		h.Logger.ErrorContext(ctx, "an integration names an organization that is not a name",
			slog.String("integration_id", integration.ID.String()),
			slog.String("error", err.Error()))
		h.counters.countDelivery(ctx, dispositionUnavailable)
		writeStatus(writer, http.StatusServiceUnavailable, "not recorded")
		return
	}

	adapter, ok := h.Adapters[integration.Type]
	if !ok {
		// The Integration names a type this build cannot parse, which is a deployment
		// configured by a newer version. It is ours, not the caller's, so it is retryable.
		h.Logger.ErrorContext(ctx, "integration names a type this build cannot parse",
			slog.String("org_id", organization.String()),
			slog.String("integration_id", integration.ID.String()),
			slog.Int("type", int(integration.Type)))
		h.counters.countDelivery(ctx, dispositionUnavailable)
		writeStatus(writer, http.StatusServiceUnavailable, "integration not served here")
		return
	}

	body, err := readBody(writer, request)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.recordRefusal(ctx, integration, storage.RefusedOversized)
			h.refuse(ctx, request, "oversized")
			h.counters.countDelivery(ctx, dispositionOversized)
			writeStatus(writer, http.StatusRequestEntityTooLarge, "payload too large")
			return
		}
		// The body did not arrive whole — a severed connection, a client that gave up.
		// Nothing was written and there is very likely nobody left to read the answer, but
		// saying so keeps the log from calling every unread body oversized.
		h.recordRefusal(ctx, integration, storage.RefusedIncomplete)
		h.refuse(ctx, request, "incomplete")
		h.counters.countDelivery(ctx, dispositionIncomplete)
		writeStatus(writer, http.StatusBadRequest, "payload not received")
		return
	}

	signals, truncated, err := adapter.Normalise(body)
	if err != nil {
		// The payload is not what this type's adapter accepts. Retrying will not change
		// that, so the status has to say permanent or the source will retry a storm of them.
		h.recordRefusal(ctx, integration, storage.RefusedMalformed)
		h.refuse(ctx, request, "malformed")
		h.counters.countDelivery(ctx, dispositionMalformed)
		writeStatus(writer, http.StatusBadRequest, "payload not understood")
		return
	}

	digest := sha256.Sum256(body)
	h.record(ctx, writer, organization, storage.Delivery{
		Integration: integration.ID,
		BodyDigest:  digest[:],
		Truncated:   truncated,
		Signals:     signals,
	})
}

// record commits the delivery and answers the source.
func (h *surface) record(
	ctx context.Context, writer http.ResponseWriter,
	organization tenancy.Organization, delivery storage.Delivery,
) {
	outcome, err := h.Database.RecordDelivery(ctx, organization, delivery)
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
		h.recordAttempt(ctx, organization, delivery.Integration, storage.DeliveryDuplicate)
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
	h.counters.work.Count(ctx, "accepted")
	h.counters.countSignals(ctx, outcome.Recorded, outcome.EpisodesOpened, outcome.EpisodesJoined)
	h.Logger.InfoContext(ctx, "delivery accepted",
		slog.String("org_id", organization.String()),
		slog.String("integration_id", delivery.Integration.String()),
		slog.Int("signals", outcome.Recorded),
		slog.Int("episodes_opened", outcome.EpisodesOpened),
		slog.Int("episodes_joined", outcome.EpisodesJoined))
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
func (h *surface) authenticate(
	ctx context.Context, integrationID uuid.UUID, request *http.Request,
) (integrations.Integration, error) {
	integration, err := h.Database.IntegrationByID(ctx, integrationID)
	switch {
	case errors.Is(err, integrations.ErrUnknown):
		return integrations.Integration{}, fmt.Errorf("%w: no such integration", errNotAuthenticated)
	case err != nil:
		return integrations.Integration{}, err
	}

	// The row is returned alongside every refusal below, so a rejection can be recorded
	// against the Integration it was aimed at. That is what makes a source delivering with
	// a stale secret VISIBLE as a rejection instead of as silence — and it is safe because
	// the row was found by primary key, not by anything the caller asserted about a tenant.
	presented := request.Header.Get(TokenHeader)
	if presented == "" || len(presented) > maxPresentedSecret {
		return integration, fmt.Errorf("%w: no usable credential presented", errNotAuthenticated)
	}

	digest := sha256.Sum256([]byte(presented))
	// Compared before the state checks below, so a disabled Integration or one that
	// receives no webhooks takes the same work as a live one and the timing says nothing
	// either. An Integration with no secret compares against an empty digest and cannot
	// match.
	matches := subtle.ConstantTimeCompare(digest[:], integration.WebhookSecretDigest) == 1

	switch {
	case !matches:
		return integration, fmt.Errorf("%w: credential does not match", errNotAuthenticated)
	case integration.Disabled():
		// An operator who turned an Integration off wants deliveries refused, not merely
		// recorded.
		return integration, fmt.Errorf("%w: integration is disabled", errNotAuthenticated)
	}
	return integration, nil
}

// callerOf reports where a delivery came from. It is what makes a campaign of credential
// guesses investigable: this surface has no operator identity behind it, so an address is the
// whole of the attribution available.
func callerOf(request *http.Request) string {
	return request.RemoteAddr
}

// addressed resolves the Integration named in the path.
//
// A path that does not parse is answered exactly as a wrong secret is: same status, same body.
// Anything else lets a caller separate "this is not the shape of an identifier" from "this is
// not an integration", and probing the first is how you learn to probe the second.
func (h *surface) addressed(
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
// No organization is named, because at this point none is known: a refused delivery never
// authenticated, so there is no tenant to attribute it to. Recording the identifier it aimed at
// is what makes a campaign against one Integration visible.
//
// Nothing the caller sent in a header is recorded. A refused delivery's headers are the one
// place guaranteed to hold a guess at the credential.
func (h *surface) refuse(ctx context.Context, request *http.Request, reason string) {
	h.Logger.WarnContext(ctx, "delivery refused",
		slog.String("integration_id", request.PathValue("integration")),
		slog.String("caller", callerOf(request)),
		slog.String("reason", reason))
}

// recordRefusal puts a rejected delivery in the Integration's own history, so an operator
// can see that a source is delivering and being turned away.
//
// Without it, the two states an operator most needs to tell apart are identical from the
// console: a source that has gone quiet and a source that is delivering every thirty
// seconds with a stale secret both show no accepted deliveries. One of those is a quiet
// night and the other is a broken intake, and they call for opposite actions at three in
// the morning.
//
// It is skipped when no Integration was found, which is both correct and what bounds it:
// there is no tenant to attribute an unknown identifier to, so an attacker guessing
// identifiers writes no rows at all. A rate-limited delivery is likewise not recorded —
// the limiter runs before the Integration is resolved, deliberately, because that is the
// defence that has to hold when everything after it is being abused.
//
// A failure to record is logged and does not change the answer. The delivery was already
// refused and telling the source something different because our own history could not be
// written would be reporting our problem as theirs.
func (h *surface) recordRefusal(
	ctx context.Context, found integrations.Integration, reason string,
) {
	if found.ID == uuid.Nil || found.OrgID == "" {
		return
	}
	organization, err := tenancy.NewOrganization(found.OrgID)
	if err != nil {
		return
	}
	if err := h.Database.RecordDeliveryAttempt(ctx, organization, storage.DeliveryAttempt{
		Integration: found.ID,
		Disposition: storage.DeliveryRejected,
		Reason:      reason,
	}); err != nil {
		h.Logger.ErrorContext(ctx, "a refused delivery could not be recorded",
			slog.String("integration_id", found.ID.String()),
			slog.String("reason", reason),
			slog.String("error", err.Error()))
	}
}

// recordAttempt puts a duplicate delivery in the history beside the refusals, so "last
// received" and "last accepted" are answerable separately. An accepted delivery needs no
// call here: the accepting transaction wrote its own row.
func (h *surface) recordAttempt(
	ctx context.Context, organization tenancy.Organization, integrationID uuid.UUID,
	disposition storage.DeliveryDisposition,
) {
	if err := h.Database.RecordDeliveryAttempt(ctx, organization, storage.DeliveryAttempt{
		Integration: integrationID,
		Disposition: disposition,
	}); err != nil {
		h.Logger.ErrorContext(ctx, "a delivery could not be recorded in the history",
			slog.String("integration_id", integrationID.String()),
			slog.String("error", err.Error()))
	}
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
