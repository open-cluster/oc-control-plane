package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"log/slog"
	"net/http"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/api/pagination"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/relay"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

// The fleet: counted, narrowed, ordered and paged by the database, plus the one operation that
// adds to it.
//
// What this replaces is a roster that could only be read newest-first, whole, one page at a
// time, with every question about it answered by whoever was scrolling. A platform engineer with
// a hundred relays was being asked to do the filtering in their head.

// bootstrapTokenLifetime is how long an enrolment token stays spendable. Long enough to install
// a Relay in a change window, short enough that one left in a wiki stops working before anybody
// finds it.
const bootstrapTokenLifetime = time.Hour

// fleetSpec is what the relay listing accepts. A Relay is organization-scoped, so the
// filters are over what a Relay is and does — never over a scope the record does not have.
var fleetSpec = table.Spec{
	Searchable:  true,
	Sortable:    []string{"registeredAt", "lastSeenAt", "version", "fingerprint"},
	DefaultSort: table.Sort{Field: "registeredAt", Descending: true},
	Filters:     []string{"state", "version", "capability"},
}

// listRelays reports an organization's relay identities and what is known about each.
func (h Handlers) listRelays(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	query, ok := h.query(writer, request, fleetSpec)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	roster, err := h.Database.ListRelays(ctx, principal, organization, storage.RelayQuery{
		Page:           storage.Page{Limit: query.Limit, After: query.Cursor},
		Search:         query.Search,
		State:          query.Filter("state"),
		Version:        query.Filter("version"),
		Capability:     query.Filter("capability"),
		SortField:      query.Sort.Field,
		Descending:     query.Sort.Descending,
		LivenessWindow: relay.LivenessAllowance,
	})
	if err != nil {
		h.fail(writer, request, err)
		return
	}

	h.Logger.InfoContext(ctx, "operator read a relay roster",
		slog.String("organization", organization.String()),
		slog.Int("relays", len(roster.Relays)),
		slog.String("caller", h.callerName(request)))

	relays := make([]relayView, 0, len(roster.Relays))
	for _, summary := range roster.Relays {
		relays = append(relays, viewOf(summary))
	}
	// No total. Counting a cursor-paginated fleet costs a second scan of everything the filters
	// matched, and the spec's own rule applies: a fabricated count is worse than an absent one,
	// and `null` is how "I did not answer this cheaply" is spelled.
	writeJSON(writer, http.StatusOK,
		table.Answer(relays, roster.Next, nil, h.fleetPartials()...))
}

// fleetPartials states what this build serves with no data behind it, and why.
//
// It is the mechanism that lets a console render ONE honest notice instead of a column of "Not
// reported" per row. availableVersion is the case it exists for: an operator wants to know what
// each Relay could be upgraded to (story 40), this build has no release channel to ask, and
// inventing a version string would be worse than saying so.
//
// It is declared ALWAYS, not only when no version floor is configured. It was conditional on the
// floor, and that was wrong in the direction that matters: a deployment that sets a floor gets a
// `minimumVersion` to compare against and STILL has no availableVersion, so the field went from
// declared-absent to silently absent exactly where an operator had most reason to look for it.
// A floor is what a Relay must be at least; an available version is what it could become, and no
// amount of the first supplies the second.
func (h Handlers) fleetPartials() []table.Partial {
	return []table.Partial{{
		Field: "availableVersion",
		Reason: "this deployment has no release channel configured, so there is nothing to say " +
			"what a relay could be upgraded to. Its current version is served; what is newer " +
			"than it is not something this control plane knows",
	}}
}

// fleetSummary counts an organization's relays.
//
// It exists because a hundred rows is not an assessment. Every number comes from one query, so
// the counts cannot disagree with each other the way separate reads at separate moments would —
// a summary saying eleven connected out of ten is worse than no summary.
func (h Handlers) fleetSummary(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	fleet, err := h.Database.FleetSummary(ctx, principal, organization, relay.LivenessAllowance,
		h.MinimumRelayVersion)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, fleetView{
		Total:           fleet.Total,
		Connected:       fleet.Connected,
		Disconnected:    fleet.Disconnected,
		Revoked:         fleet.Revoked,
		Outdated:        fleet.Outdated,
		Degraded:        fleet.Degraded,
		ActiveRequests:  fleet.ActiveRequests,
		LivenessSeconds: int(fleet.LivenessWindow.Seconds()),
		MinimumVersion:  fleet.MinimumVersion,
		OutdatedCounted: fleet.MinimumVersion != "",
	})
}

// relayIntegrations lists what a Relay serves, so an operator knows what disabling it
// costs before they disable it.
func (h Handlers) relayIntegrations(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, registration, ok := h.relay(writer, request)
	if !ok {
		return
	}
	query, ok := h.query(writer, request, relayIntegrationsSpec)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	list, err := h.Database.QueryIntegrations(ctx, principal, organization,
		integrations.Query{
			Page:  integrations.Page{Limit: query.Limit, After: query.Cursor},
			Relay: registration,
		})
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	served := make([]servedIntegrationView, 0, len(list.Integrations))
	for _, found := range list.Integrations {
		view := servedIntegrationView{
			ID:       found.ID.String(),
			Name:     found.Name,
			Status:   found.Status.String(),
			Disabled: found.Disabled(),
		}
		if definition, known := h.Catalog.ByID(found.Type); known {
			view.Type = definition.Key
		}
		served = append(served, view)
	}
	writeJSON(writer, http.StatusOK, table.Answer(served, list.Next, nil))
}

// failures reports what a Relay has recently failed to complete, so an intermittent one can be
// diagnosed from the record rather than from whoever happened to be watching (story 41).
//
// It says plainly what it cannot say. relay_job records that an execution failed and not what
// the Relay said about it, so the reason travels as a `partial` rather than as an empty column —
// which is the difference between an operator knowing the reason is unavailable and concluding
// there was not one.
func (h Handlers) relayFailures(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, registration, ok := h.relay(writer, request)
	if !ok {
		return
	}
	query, ok := h.query(writer, request, relayFailuresSpec)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	list, err := h.Database.RelayFailures(ctx, principal, organization, registration,
		storage.Page{Limit: query.Limit, After: query.Cursor})
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	failures := make([]relayFailureView, 0, len(list.Failures))
	for _, failure := range list.Failures {
		failures = append(failures, relayFailureView{
			JobID:             failure.JobID.String(),
			CapabilityID:      failure.CapabilityID,
			CapabilityVersion: failure.CapabilityVersion,
			IntegrationID:     failure.Integration.String(),
			Outcome:           outcomeOf(failure.Cancelled),
			At:                failure.At,
		})
	}
	writeJSON(writer, http.StatusOK, table.Answer(failures, list.Next, nil, table.Partial{
		Field: "reason",
		Reason: "a job records that it failed and not what the relay said about it, so why each " +
			"one failed is not something this control plane holds",
	}))
}

// outcomeOf names what happened, because "failed" and "cancelled" are both executions that
// produced nothing and only one of them is the Relay's fault.
func outcomeOf(cancelled bool) string {
	if cancelled {
		return "cancelled"
	}
	return "failed"
}

var relayFailuresSpec = table.Spec{
	Sortable:    []string{"at"},
	DefaultSort: table.Sort{Field: "at", Descending: true},
}

var relayIntegrationsSpec = table.Spec{
	Sortable:    []string{"createdAt"},
	DefaultSort: table.Sort{Field: "createdAt", Descending: true},
}

// issueBootstrapToken mints a single-use enrolment token and shows it once.
//
// It is the one operation on this surface that hands a credential to a caller, and it follows the
// same rule the trigger secret does: what is stored is a digest, no path reads it back, and an
// operator who loses it issues another rather than recovering this one. The expiry is stated in
// the response, because a credential with no visible lifetime is one somebody puts in a wiki.
func (h Handlers) issueBootstrapToken(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		// Refused rather than served with a weaker token. A bootstrap token enrols a Relay into
		// this tenant, and there is no acceptable fallback for the randomness behind it.
		h.Logger.ErrorContext(ctx, "a bootstrap token could not be generated",
			slog.String("error", err.Error()))
		writeJSON(writer, http.StatusServiceUnavailable,
			errorView{Error: "a token could not be generated"})
		return
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	digest := sha256.Sum256([]byte(token))
	expiresAt := time.Now().UTC().Add(bootstrapTokenLifetime)

	if err := h.Database.IssueOperatorBootstrapToken(
		ctx, principal, organization, digest[:], expiresAt); err != nil {
		h.fail(writer, request, err)
		return
	}
	// As loud as any other credential issuance. The token itself is not logged and could not
	// usefully be: what an investigation needs later is who issued one and when.
	h.Logger.WarnContext(ctx, "operator issued a relay bootstrap token",
		slog.String("organization", organization.String()),
		slog.String("actor", principal.ID()),
		slog.Time("expires_at", expiresAt),
		slog.String("caller", h.callerName(request)))

	writeJSON(writer, http.StatusCreated, bootstrapTokenView{
		Token:     token,
		ExpiresAt: expiresAt,
		Notice: "This token is shown once and cannot be read back. It enrols exactly one Relay " +
			"and is spent when it does; if it expires unused, issue another.",
	})
}

// query parses a listing's query string against what that listing serves, answering the caller
// itself on a refusal. An unknown sort or filter is refused rather than ignored: a sort silently
// dropped returns rows in an order nobody chose, and a filter silently dropped returns
// everything while looking narrowed.
func (h Handlers) query(
	writer http.ResponseWriter, request *http.Request, spec table.Spec,
) (table.Query, bool) {
	parsed, err := table.Parse(request.URL.Query(), spec)
	if err != nil {
		if table.Refused(err) {
			writeJSON(writer, http.StatusBadRequest, errorView{Error: err.Error()})
			return table.Query{}, false
		}
		h.Logger.ErrorContext(request.Context(), "a listing declares a query it cannot serve",
			slog.String("path", request.URL.Path),
			slog.String("error", err.Error()))
		writeJSON(writer, http.StatusInternalServerError, errorView{Error: "request failed"})
		return table.Query{}, false
	}
	return parsed, true
}
