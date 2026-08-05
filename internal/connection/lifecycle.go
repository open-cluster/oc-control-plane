package connection

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/relay"
	"github.com/open-cluster/oc-control-plane/internal/storage"
	"github.com/open-cluster/oc-control-plane/internal/table"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// The operations that turn a Connection from a row somebody created into something with a
// history: read it, revise it, validate it, see what has been delivered through it, test it,
// and — narrowly — remove it.
//
// The one that matters most is validate. Everything else on this surface answers "what did
// somebody configure"; validate answers "does it work", and until it existed the two were
// indistinguishable until an investigation needed the Connection and found out the hard way.

// read answers one Connection, including its credential metadata, its lifecycle state, and —
// for a trigger Connection — where its source should deliver and how that has been going.
func (h Handlers) read(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, id, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	if !principal.MemberOf(organization) {
		// The same answer the middleware gives. Reaching storage first would be a second place
		// the boundary is decided.
		h.fail(writer, request, storage.ErrNotAMember)
		return
	}
	found, err := h.Placements.ConnectionForOrganization(ctx, organization, id)
	if err != nil {
		h.fail(writer, request, err)
		return
	}

	view := detailViewOf(found, h.deliveryEndpoint(found))
	if found.Role.Includes(storage.RoleTrigger) {
		health, healthErr := h.Placements.TriggerDeliveryHealth(ctx, principal, organization, id)
		if healthErr != nil {
			h.fail(writer, request, healthErr)
			return
		}
		view.Trigger = triggerViewOf(health)
	}
	writeJSON(writer, http.StatusOK, view)
}

// revise applies a change to a Connection's configuration and increments its revision.
//
// The credential is deliberately not revisable here. Rotating one is its own operation with its
// own permission and its own audit event, and folding it into a general edit would make "I
// changed the endpoint" and "I replaced the credential" the same line in the record.
func (h Handlers) revise(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, id, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	var body reviseRequest
	if !decode(writer, request, &body) {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	if !principal.MemberOf(organization) {
		h.fail(writer, request, storage.ErrNotAMember)
		return
	}
	existing, err := h.Placements.ConnectionForOrganization(ctx, organization, id)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	definition, known := Lookup(Integration(existing.Integration))
	if !known {
		// A row naming an Integration this build no longer has. It is ours rather than the
		// caller's — a deployment configured by a newer version — so it is not a 400.
		h.Logger.ErrorContext(ctx, "a connection names an integration this build does not have",
			slog.String("connection_id", id.String()),
			slog.String("integration", existing.Integration))
		writeJSON(writer, http.StatusInternalServerError, errorView{Error: "request failed"})
		return
	}

	configuration, ok := h.checkedConfiguration(writer, definition, body.Configuration)
	if !ok {
		return
	}
	labels, ok := validLabels(writer, body.Labels)
	if !ok {
		return
	}

	revised, err := h.Placements.ReviseConnection(ctx, principal, organization, id,
		configuration, labels)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	h.Logger.InfoContext(ctx, "operator revised a connection",
		slog.String("organization", organization.String()),
		slog.String("connection_id", id.String()),
		slog.Int("configuration_revision", revised.ConfigurationRevision),
		slog.String("caller", h.callerName(request)))

	writeJSON(writer, http.StatusOK, detailViewOf(revised, h.deliveryEndpoint(revised)))
}

// remove deletes a Connection nothing depends on, and tells the caller what depends on it when
// something does.
//
// A refusal here is not a failure — it is the answer to story 25. The dependents travel with it
// so an operator learns the cost in the same response that declines to impose it, and the next
// thing they reach for is disabling, which loses nothing.
func (h Handlers) remove(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, id, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	err := h.Placements.DeleteConnection(ctx, principal, organization, id)
	if errors.Is(err, storage.ErrConnectionHasDependents) {
		// Read back what blocked it, so the refusal carries the cost rather than only the
		// verdict. The delete decided under a row lock and committed nothing; this is a second
		// read on an uncommon path, and it is as-of-now, which is what an operator is reading.
		blocking, countErr := h.Placements.ConnectionDependents(ctx, principal, organization, id)
		if countErr != nil {
			h.fail(writer, request, countErr)
			return
		}
		writeJSON(writer, http.StatusConflict, dependentsView{
			Error: "this connection has a history and other records point at it, so removing it " +
				"would destroy them; disable it instead, which stops it participating and keeps " +
				"the record of what it produced",
			Dependents: dependentCountsOf(blocking),
		})
		return
	}
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	h.Logger.WarnContext(ctx, "operator deleted a connection",
		slog.String("organization", organization.String()),
		slog.String("connection_id", id.String()),
		slog.String("caller", h.callerName(request)))
	writer.WriteHeader(http.StatusNoContent)
}

// dependents reports what depends on a Connection without changing anything, so an operator can
// find out before they try rather than by trying.
func (h Handlers) dependents(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, id, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	found, err := h.Placements.ConnectionDependents(ctx, principal, organization, id)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, dependentsView{Dependents: dependentCountsOf(found)})
}

// validate exercises a Connection and records what it found.
//
// The result is PER CAPABILITY rather than a boolean, which is the whole design. "Authentication
// worked, two of five capabilities are unavailable" is something an operator can act on;
// collapsed into pass or fail it becomes a failure, and a failure is what somebody retries
// rather than reads.
//
// What each provider's validation actually establishes is stated in its definition's validation
// contract and travels back with the result, so nobody has to guess how much a green tick is
// worth.
func (h Handlers) validate(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, id, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	if !principal.MemberOf(organization) {
		h.fail(writer, request, storage.ErrNotAMember)
		return
	}
	found, err := h.Placements.ConnectionForOrganization(ctx, organization, id)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	definition, known := Lookup(Integration(found.Integration))
	if !known {
		h.Logger.ErrorContext(ctx, "a connection names an integration this build does not have",
			slog.String("connection_id", id.String()),
			slog.String("integration", found.Integration))
		writeJSON(writer, http.StatusInternalServerError, errorView{Error: "request failed"})
		return
	}

	result, err := h.check(ctx, organization, found, definition)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	recorded, err := h.Placements.RecordValidation(ctx, principal, organization, id, result)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	h.Logger.InfoContext(ctx, "operator validated a connection",
		slog.String("organization", organization.String()),
		slog.String("connection_id", id.String()),
		slog.String("outcome", recorded.Outcome.String()),
		slog.String("caller", h.callerName(request)))

	writeJSON(writer, http.StatusOK, validationViewOf(recorded))
}

// check is what validation actually does, and it is deliberately modest about it.
//
// It establishes what can be established WITHOUT dispatching work into a customer's systems: a
// trigger Connection is checked for being able to receive at all, and an evidence Connection is
// checked against the Relay bound to it — does that Relay exist, is it connected, and does it
// advertise each capability this provider declares. A Relay too old to compile a capability will
// refuse the job when it arrives, and finding that out during an investigation is finding it out
// during an incident.
//
// What it does NOT establish is whether the customer's own authorization permits the read. That
// is answered by the first real read and by nothing before it, and the validation contract that
// travels with the result says so rather than leaving a green tick to imply otherwise.
func (h Handlers) check(
	ctx context.Context, organization tenancy.Organization, found storage.Connection,
	definition Definition,
) (storage.ValidationResult, error) {
	result := storage.ValidationResult{
		Note:      definition.Validation.Note,
		StartedAt: time.Now().UTC(),
	}

	if found.Disabled() {
		// NOT a failure. Being turned off is orthogonal to working — that is why disabled_at is
		// a separate column from the state — and recording `failed` here would tell an operator
		// a check found something wrong when no check ran. Nothing was attempted, so nothing is
		// claimed, and the Connection keeps whatever the last real check established.
		result.Authentication = storage.ReadyNotAttempted
		result.AuthenticationReason = "this connection is disabled, so nothing was run through " +
			"it; enable it to find out whether it works"
		result.Capabilities = notAttempted(definition, "the connection is disabled")
		return result, nil
	}

	if !definition.Validation.Authenticates {
		// Reached inbound only. There is nothing here to authenticate against, and a pass would
		// be a green tick that meant nothing — which is worse than an honest "not attempted",
		// because somebody would act on it.
		result.Authentication = storage.ReadyNotAttempted
		result.AuthenticationReason = "this provider is reached inbound only; its credential is " +
			"verified when a delivery arrives, not when one is configured"
		if found.Role.Includes(storage.RoleTrigger) && len(found.SecretDigest) == 0 {
			result.Authentication = storage.ReadyUnavailable
			result.AuthenticationReason = "this trigger connection holds no verification secret, " +
				"so it could not tell a real delivery from any other"
		}
		result.Capabilities = notAttempted(definition,
			"this provider declares no outbound capabilities")
		return result, nil
	}

	if found.Locality != storage.LocalityRelay {
		// A central evidence Connection. Nothing in this build reaches one — every provider that
		// would be reached centrally is `planned` — so saying so is the honest answer rather
		// than a pass.
		result.Authentication = storage.ReadyNotAttempted
		result.AuthenticationReason = "this connection is reached centrally, and this build has " +
			"no adapter that reaches it"
		result.Capabilities = notAttempted(definition, "no adapter reaches this provider centrally")
		return result, nil
	}

	relay, err := h.Placements.RelayServing(ctx, organization, found.RelayRegistration,
		relay.LivenessAllowance)
	if err != nil {
		return storage.ValidationResult{}, err
	}
	switch {
	case !relay.Registered:
		result.Authentication = storage.ReadyUnavailable
		result.AuthenticationReason = "the relay this connection is bound to is not registered, " +
			"or its registration has been revoked"
		result.Capabilities = notAttempted(definition, "the relay serving this is not registered")
		return result, nil
	case !relay.Connected:
		result.Authentication = storage.ReadyUnavailable
		result.AuthenticationReason = "the relay this connection is bound to is not connected, " +
			"so nothing can run through it right now"
		result.Capabilities = notAttempted(definition, "the relay serving this is not connected")
		return result, nil
	}

	result.Authentication = storage.ReadyAvailable
	result.AuthenticationReason = "the relay serving this connection is registered and connected"
	result.Capabilities = advertisedReadiness(definition, relay)
	return result, nil
}

// notAttempted marks every declared capability as unchecked, with the one reason nothing was
// checked. It is NOT a pass, and it is not a failure either: it is the record saying what was
// skipped, which an operator reading a clean result deserves to be able to tell.
func notAttempted(definition Definition, reason string) []storage.CapabilityReadiness {
	readiness := make([]storage.CapabilityReadiness, 0, len(definition.Capabilities))
	for _, named := range definition.Capabilities {
		readiness = append(readiness, storage.CapabilityReadiness{
			Capability: named,
			Readiness:  storage.ReadyNotAttempted,
			Reason:     reason,
		})
	}
	return readiness
}

// advertisedReadiness checks each declared capability against what the bound Relay said it could
// run at enrolment. A capability the Relay does not advertise is one it will refuse when the job
// arrives, which is exactly the partial result this design exists to be able to report.
func advertisedReadiness(
	definition Definition, relay storage.RelayAdvertisement,
) []storage.CapabilityReadiness {
	readiness := make([]storage.CapabilityReadiness, 0, len(definition.Capabilities))
	for _, named := range definition.Capabilities {
		entry := storage.CapabilityReadiness{Capability: named, Readiness: storage.ReadyAvailable}
		if !relay.Capabilities[named] {
			entry.Readiness = storage.ReadyUnavailable
			entry.Reason = "the relay serving this connection runs " + relay.Version +
				" and does not advertise this capability"
		}
		readiness = append(readiness, entry)
	}
	return readiness
}

// validationsSpec is what the validation history accepts. It is ordered by when a run finished
// and nothing else: a history a caller could reorder would be a history whose "most recent" is
// whatever they asked for.
var validationsSpec = table.Spec{
	Sortable:    []string{"completedAt"},
	DefaultSort: table.Sort{Field: "completedAt", Descending: true},
}

// validations reads what checking this Connection has found over time, which is what tells an
// outage from a configuration that never worked.
func (h Handlers) validations(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, id, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	query, ok := h.query(writer, request, validationsSpec)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	list, err := h.Placements.ConnectionValidations(ctx, principal, organization, id,
		storage.Page{Limit: query.Limit, After: query.Cursor}, query.Sort.Descending)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	views := make([]validationView, 0, len(list.Validations))
	for _, recorded := range list.Validations {
		views = append(views, validationViewOf(recorded))
	}
	// No total. Counting a history that grows every time somebody presses the button costs a
	// scan, and a count nobody asked for is not worth what it costs on the one page that is read
	// during an incident.
	writeJSON(writer, http.StatusOK, table.Answer(views, list.Next, nil))
}

// deliveriesSpec is what the delivery history accepts.
var deliveriesSpec = table.Spec{
	Sortable:    []string{"receivedAt"},
	DefaultSort: table.Sort{Field: "receivedAt", Descending: true},
	Filters:     []string{"outcome"},
}

// deliveries reads what has arrived through this Connection, so a missed investigation can be
// correlated with a missed delivery rather than guessed at.
func (h Handlers) deliveries(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, id, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	query, ok := h.query(writer, request, deliveriesSpec)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	outcome, ok := h.deliveryOutcome(writer, query.Filter("outcome"))
	if !ok {
		return
	}
	list, err := h.Placements.TriggerDeliveries(ctx, principal, organization, id,
		storage.DeliveryQuery{
			Page:       storage.Page{Limit: query.Limit, After: query.Cursor},
			Outcome:    outcome,
			Descending: query.Sort.Descending,
		})
	if err != nil {
		h.fail(writer, request, err)
		return
	}

	views := make([]deliveryView, 0, len(list.Deliveries))
	for _, delivery := range list.Deliveries {
		views = append(views, deliveryViewOf(delivery))
	}
	writeJSON(writer, http.StatusOK, table.Answer(views, list.Next, nil))
}

// deliveryOutcome resolves the outcome a caller narrowed to. An unrecognised one is REFUSED
// rather than narrowing to nothing: an empty page is what "there were none" looks like, and a
// caller who misspelled `rejcted` would read it as a Connection with no refusals.
func (h Handlers) deliveryOutcome(
	writer http.ResponseWriter, named string,
) (storage.DeliveryDisposition, bool) {
	switch named {
	case "":
		return 0, true
	case "accepted":
		return storage.DeliveryAccepted, true
	case "duplicate":
		return storage.DeliveryDuplicate, true
	case "rejected":
		return storage.DeliveryRejected, true
	default:
		writeJSON(writer, http.StatusBadRequest, errorView{
			Error: `outcome must be "accepted", "duplicate" or "rejected"`})
		return 0, false
	}
}

// testEvent records an operator's own probe of a trigger Connection.
//
// It is honest about what it proves, in the response rather than in a document nobody reads. It
// establishes that this Connection is one a delivery could be recorded against — it exists, it
// is enabled, it answers triggers, and the recording path works. It does NOT establish that the
// customer's own system can reach the endpoint, because nothing this platform does to itself can
// establish that. What proves the other half is a delivery from the source, and the response
// says which half it just proved.
func (h Handlers) testEvent(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, id, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	if err := h.Placements.RecordTestDelivery(ctx, principal, organization, id); err != nil {
		if errors.Is(err, storage.ErrConnectionUnknown) {
			writeJSON(writer, http.StatusConflict, errorView{
				Error: "this connection cannot receive a delivery: it does not exist, it is " +
					"disabled, or it answers no triggers",
			})
			return
		}
		h.fail(writer, request, err)
		return
	}
	h.Logger.InfoContext(ctx, "operator sent a test event",
		slog.String("organization", organization.String()),
		slog.String("connection_id", id.String()),
		slog.String("caller", h.callerName(request)))

	writeJSON(writer, http.StatusAccepted, testEventView{
		Recorded: true,
		Proves: "this connection exists, is enabled, answers triggers, and a delivery through " +
			"it is recorded. It is marked as a test in the delivery history.",
		DoesNotProve: "that your own system can reach the delivery endpoint. Nothing this " +
			"platform sends to itself can establish that; a delivery from the source is what does.",
	})
}

// deliveryEndpoint is where a trigger Connection's source should post.
//
// It is empty when the deployment has not been told what URL it is reachable at, and empty is
// what gets served — a guess assembled from the request's own Host header would be a URL that
// works from wherever the console happens to be and not from the customer's alerting, which is
// the one place it has to work.
func (h Handlers) deliveryEndpoint(found storage.Connection) string {
	if h.IntakeBaseURL == "" || !found.Role.Includes(storage.RoleTrigger) {
		return ""
	}
	return h.IntakeBaseURL + "/intake/v1/connections/" + found.ID.String() + "/signals"
}

// dependentCountsOf renders what depends on a Connection.
func dependentCountsOf(found storage.Dependents) dependentCounts {
	return dependentCounts{
		Signals:                   found.Signals,
		Deliveries:                found.Deliveries,
		Jobs:                      found.Jobs,
		LastEvidenceInEnvironment: found.LastEvidenceInEnvironment,
		Any:                       found.Any(),
	}
}
