package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/api/listing"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/relay"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

const bootstrapTokenLifetime = time.Hour

var fleetSpec = listing.Spec{
	Searchable:  true,
	Sortable:    []string{"registeredAt", "lastSeenAt", "version", "fingerprint"},
	DefaultSort: listing.Sort{Field: "registeredAt", Descending: true},
	Filters:     []string{"state", "version", "capability"},
}

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
	state := query.Filter("state")
	if state != "" && state != "connected" && state != "disconnected" &&
		state != "revoked" && state != "degraded" {
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: "state must be connected, disconnected, revoked, or degraded"})
		return
	}
	for _, name := range []string{"version", "capability"} {
		value := query.Filter(name)
		if len(value) > 128 || value != strings.TrimSpace(value) {
			writeJSON(writer, http.StatusBadRequest,
				errorView{Error: name + " must be at most 128 characters without surrounding whitespace"})
			return
		}
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	roster, err := h.Database.ListRelays(ctx, principal, organization, storage.RelayQuery{
		Page:           storage.Page{Limit: query.Limit, After: query.Cursor},
		Search:         query.Search,
		State:          state,
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
	writeJSON(writer, http.StatusOK,
		listing.Answer(relays, roster.Next, nil, h.fleetPartials()...))
}

func (h Handlers) fleetPartials() []listing.Partial {
	return []listing.Partial{{
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
	writeJSON(writer, http.StatusOK, listing.Answer(served, list.Next, nil))
}

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
	writeJSON(writer, http.StatusOK, listing.Answer(failures, list.Next, nil, listing.Partial{
		Field: "reason",
		Reason: "a job records that it failed and not what the relay said about it, so why each " +
			"one failed is not something this control plane holds",
	}))
}

func outcomeOf(cancelled bool) string {
	if cancelled {
		return "cancelled"
	}
	return "failed"
}

var relayFailuresSpec = listing.Spec{
	DefaultSort: listing.Sort{Field: "at", Descending: true},
}

var relayIntegrationsSpec = listing.Spec{
	DefaultSort: listing.Sort{Field: "createdAt", Descending: true},
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
	writer http.ResponseWriter, request *http.Request, spec listing.Spec,
) (listing.Query, bool) {
	parsed, err := listing.Parse(request.URL.Query(), spec)
	if err != nil {
		if listing.Refused(err) {
			writeJSON(writer, http.StatusBadRequest, errorView{Error: err.Error()})
			return listing.Query{}, false
		}
		h.Logger.ErrorContext(request.Context(), "a listing declares a query it cannot serve",
			slog.String("path", request.URL.Path),
			slog.String("error", err.Error()))
		writeJSON(writer, http.StatusInternalServerError, errorView{Error: "request failed"})
		return listing.Query{}, false
	}
	return parsed, true
}
