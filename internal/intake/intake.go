// Package intake accepts alerts from the systems a customer already runs.
//
// The product does not detect. Building an alerting engine would rebuild what every customer
// already has and would contradict the position that this is an investigation platform rather
// than a monitoring one. What customers do not have is anything that starts investigating
// before a human arrives, and reaching that means accepting their alerts rather than replacing
// them.
//
// Accepting them is not a thin adapter. Each system has its own payload, its own idea of
// authentication, its own retry behaviour and its own notion of what counts as the same alert
// firing twice. Every one of those differences is confined to an adapter here; past
// normalisation nothing can tell which system delivered a Signal, and that boundary is what
// keeps adding the second source cheap.
//
// The surface is deliberately its own listener. It is the only part of the control plane a
// customer's infrastructure connects to inbound, so a deployment can expose it and nothing
// else — health, metrics, the operator surface and the relay endpoint all stay where they are.
package intake

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

	"github.com/open-cluster/oc-control-plane/internal/storage"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// TokenHeader carries the source's shared secret.
//
// It is a header rather than a signature because the first source cannot sign: Alertmanager
// attaches static headers to a webhook and nothing more. The consequence is written down in
// the specification rather than left implicit — this authenticates the sender and attests
// nothing about the body — and verification is per-adapter precisely so a source that can sign
// gets a signature instead.
const TokenHeader = "X-OpenCluster-Token" //nolint:gosec // a header name, not a credential.

// maxBodyBytes bounds a delivery. It is enforced as the body is read rather than after, so an
// oversized payload is refused without ever being held whole — intake is reachable by anything
// that can guess a source identifier, and a size bound applied after buffering is not a bound.
const maxBodyBytes = 1 << 20

// readTimeout bounds how long one delivery may take. A source that opens a connection and
// then goes quiet must not be able to hold it.
const readTimeout = 15 * time.Second

// Handlers is the intake surface's dependencies.
type Handlers struct {
	Placements *storage.Placements
	Logger     *slog.Logger
}

// Router returns the intake surface.
func (h Handlers) Router() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /intake/v1/organizations/{organization}/sources/{source}",
		http.HandlerFunc(h.deliver))
	return mux
}

// deliver accepts one webhook delivery.
//
// The order is the point. Authentication happens before the body is touched, so an
// unauthenticated caller reaches none of the parser — which is the part that handles input
// nobody has vouched for. Only then is the body read, under a bound, and only then parsed.
func (h Handlers) deliver(writer http.ResponseWriter, request *http.Request) {
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	organization, sourceID, ok := h.addressed(writer, request)
	if !ok {
		return
	}

	source, err := h.authenticate(ctx, organization, sourceID, request)
	if err != nil {
		// A failure to READ the source is not a failure to authenticate, and answering it as
		// one would be the worst mistake available here: 401 is permanent, so a database
		// outage would tell every source to give up, and the alerts they would otherwise have
		// retried are gone for good. Only a source that was read and did not match is refused.
		if !errors.Is(err, errNotAuthenticated) {
			h.Logger.ErrorContext(ctx, "could not read the alert source",
				slog.String("organization", organization.String()),
				slog.String("caller", callerOf(request)),
				slog.String("error", err.Error()))
			writeStatus(writer, http.StatusServiceUnavailable, "not recorded")
			return
		}
		h.refuse(ctx, request, organization, "unauthenticated")
		// One status and one message however it failed. A missing header, a wrong secret, an
		// unknown source and a disabled one are indistinguishable, because telling them apart
		// is how a caller learns which half of a guess was right.
		writeStatus(writer, http.StatusUnauthorized, "unauthorized")
		return
	}

	adapter, ok := adapterFor(source.Kind)
	if !ok {
		// The source names an adapter this build does not have, which is a deployment that was
		// configured by a newer version. It is ours, not the caller's, so it is retryable.
		h.Logger.ErrorContext(ctx, "alert source names an unknown adapter",
			slog.String("organization", organization.String()),
			slog.String("source_id", source.ID.String()),
			slog.String("kind", source.Kind))
		writeStatus(writer, http.StatusServiceUnavailable, "source kind not served here")
		return
	}

	body, err := readBody(writer, request)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.refuse(ctx, request, organization, "oversized")
			writeStatus(writer, http.StatusRequestEntityTooLarge, "payload too large")
			return
		}
		// The body did not arrive whole — a severed connection, a client that gave up. Nothing
		// was written and there is very likely nobody left to read the answer, but saying so
		// keeps the log from calling every unread body oversized.
		h.refuse(ctx, request, organization, "incomplete")
		writeStatus(writer, http.StatusBadRequest, "payload not received")
		return
	}

	normalised, err := adapter.Normalise(body)
	if err != nil {
		// The payload is not what this source's adapter accepts. Retrying will not change
		// that, so the status has to say permanent or the source will retry a storm of them.
		h.refuse(ctx, request, organization, "malformed")
		writeStatus(writer, http.StatusBadRequest, "payload not understood")
		return
	}

	digest := sha256.Sum256(body)
	h.record(ctx, writer, organization, storage.Delivery{
		Source:     source.ID,
		BodyDigest: digest[:],
		Truncated:  normalised.Truncated,
		Signals:    normalised.Signals,
	})
}

// record commits the delivery and answers the source.
func (h Handlers) record(
	ctx context.Context, writer http.ResponseWriter,
	organization tenancy.Organization, delivery storage.Delivery,
) {
	outcome, err := h.Placements.RecordDelivery(ctx, organization, delivery)
	if err != nil {
		// Nothing was written, and the source should try again — this is the one failure that
		// is genuinely ours and genuinely transient.
		h.Logger.ErrorContext(ctx, "recording a delivery failed",
			slog.String("organization", organization.String()),
			slog.String("source_id", delivery.Source.String()),
			slog.String("error", err.Error()))
		writeStatus(writer, http.StatusServiceUnavailable, "not recorded")
		return
	}

	if outcome.Duplicate {
		// A source retrying because it never saw a response has done nothing wrong, and the
		// answer must let it stop rather than inviting the retry again.
		h.Logger.InfoContext(ctx, "delivery already accepted",
			slog.String("organization", organization.String()),
			slog.String("source_id", delivery.Source.String()))
		writeStatus(writer, http.StatusOK, "already accepted")
		return
	}

	// A truncated delivery is recorded and reported, not refused. The alerts that did arrive
	// are real and refusing would lose them, since the source will not send them again — but a
	// truncation is the sender saying this platform's record of the moment is incomplete, and
	// that must be visible rather than inferred from a count that looks fine.
	if delivery.Truncated > 0 {
		h.Logger.WarnContext(ctx, "the source truncated this delivery",
			slog.String("organization", organization.String()),
			slog.String("source_id", delivery.Source.String()),
			slog.Int("omitted", delivery.Truncated))
	}

	h.Logger.InfoContext(ctx, "delivery accepted",
		slog.String("organization", organization.String()),
		slog.String("source_id", delivery.Source.String()),
		slog.Int("signals", outcome.Recorded))
	writeStatus(writer, http.StatusAccepted, "accepted")
}

// errNotAuthenticated marks the failures that are the caller's: no credential, a wrong one, a
// source that does not exist or has been turned off. Everything else reaching the caller of
// authenticate is ours, and must not be answered as a refusal.
var errNotAuthenticated = errors.New("not authenticated")

// authenticate resolves the source and checks the secret it was configured with.
func (h Handlers) authenticate(
	ctx context.Context, organization tenancy.Organization,
	sourceID uuid.UUID, request *http.Request,
) (storage.AlertSource, error) {
	presented := request.Header.Get(TokenHeader)
	if presented == "" {
		return storage.AlertSource{}, fmt.Errorf("%w: no credential presented", errNotAuthenticated)
	}

	source, err := h.Placements.AlertSourceByID(ctx, organization, sourceID)
	switch {
	case errors.Is(err, storage.ErrUnknownSource),
		errors.Is(err, storage.ErrUnknownOrganization):
		// An organization this instance serves no placement for cannot own a source, so it is
		// the same answer as an unknown one rather than a hint about how tenants are placed.
		return storage.AlertSource{}, fmt.Errorf("%w: no such source", errNotAuthenticated)
	case err != nil:
		return storage.AlertSource{}, err
	}

	digest := sha256.Sum256([]byte(presented))
	if subtle.ConstantTimeCompare(digest[:], source.SecretDigest) != 1 {
		return storage.AlertSource{}, fmt.Errorf("%w: credential does not match", errNotAuthenticated)
	}
	return source, nil
}

// callerOf reports where a delivery came from. It is what makes a campaign of credential
// guesses investigable: this surface has no operator identity behind it, so an address is the
// whole of the attribution available.
func callerOf(request *http.Request) string {
	return request.RemoteAddr
}

// addressed resolves the tenant and source named in the path.
//
// A path that does not parse is answered exactly as a wrong secret is: same status, same body.
// Anything else lets a caller separate "this is not the shape of a source identifier" from
// "this is not a source", and probing the first is how you learn to probe the second.
func (h Handlers) addressed(
	writer http.ResponseWriter, request *http.Request,
) (tenancy.Organization, uuid.UUID, bool) {
	organization, err := tenancy.NewOrganization(request.PathValue("organization"))
	if err != nil {
		writeStatus(writer, http.StatusUnauthorized, "unauthorized")
		return tenancy.Organization{}, uuid.UUID{}, false
	}
	sourceID, err := uuid.Parse(request.PathValue("source"))
	if err != nil {
		writeStatus(writer, http.StatusUnauthorized, "unauthorized")
		return tenancy.Organization{}, uuid.UUID{}, false
	}
	return organization, sourceID, true
}

// readBody reads the delivery under its bound. MaxBytesReader stops at the limit rather than
// after it, so an oversized payload is never held whole.
func readBody(writer http.ResponseWriter, request *http.Request) ([]byte, error) {
	limited := http.MaxBytesReader(writer, request.Body, maxBodyBytes)
	return io.ReadAll(limited)
}

// refuse records a rejected delivery: its reason, its source and where it came from, and never
// its payload. The body is untrusted text from a customer's systems, and a log that quoted it
// would turn diagnosis into a disclosure channel. The caller's address is recorded for the
// opposite reason — without it a campaign of credential guesses leaves nothing to investigate.
//
// Nothing the caller sent in a header is recorded. A refused delivery's headers are the one
// place guaranteed to hold a guess at the credential.
func (h Handlers) refuse(
	ctx context.Context, request *http.Request,
	organization tenancy.Organization, reason string,
) {
	h.Logger.WarnContext(ctx, "delivery refused",
		slog.String("organization", organization.String()),
		slog.String("source_id", request.PathValue("source")),
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
// Nothing this surface returns may be cached: an answer holds a named tenant's source in its
// URL, and an intermediary holding one operator's response is a cross-tenant disclosure
// waiting for the next request.
func writeStatus(writer http.ResponseWriter, code int, status string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(code)
	_ = json.NewEncoder(writer).Encode(statusBody{Status: status})
}
