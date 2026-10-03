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

var relayListSpec = listing.Spec{
	Searchable:  true,
	Sortable:    []string{"registeredAt", "lastSeenAt", "version", "fingerprint"},
	DefaultSort: listing.Sort{Field: "registeredAt", Descending: true},
	Filters:     []string{"state", "version", "capability"},
}

func (h Handlers) listRelays(writer http.ResponseWriter, request *http.Request) {
	principal := h.caller(request)
	organization := h.organization(request)
	query, ok := h.query(writer, request, relayListSpec)
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
	writeJSON(writer, http.StatusOK, listing.NewPage(relays, roster.Next, nil))
}

func (h Handlers) relaySummary(writer http.ResponseWriter, request *http.Request) {
	principal := h.caller(request)
	organization := h.organization(request)
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	summary, err := h.Database.CountRelays(ctx, principal, organization, relay.LivenessAllowance)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, relaySummaryView{
		Total:           summary.Total,
		Connected:       summary.Connected,
		Disconnected:    summary.Disconnected,
		Revoked:         summary.Revoked,
		Degraded:        summary.Degraded,
		ActiveRequests:  summary.ActiveRequests,
		LivenessSeconds: int(summary.LivenessWindow.Seconds()),
	})
}

func (h Handlers) relayIntegrations(writer http.ResponseWriter, request *http.Request) {
	principal := h.caller(request)
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
			Disabled: found.Disabled,
		}
		if status := found.Status.String(); status != "" {
			view.Status = &status
		}
		if definition, known := h.Catalog.Lookup(found.Provider); known {
			view.Type = string(definition.Key)
		}
		served = append(served, view)
	}
	writeJSON(writer, http.StatusOK, listing.NewPage(served, list.Next, nil))
}

func (h Handlers) relayFailures(writer http.ResponseWriter, request *http.Request) {
	principal := h.caller(request)
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
	writeJSON(writer, http.StatusOK, listing.NewPage(failures, list.Next, nil))
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

func (h Handlers) issueBootstrapToken(writer http.ResponseWriter, request *http.Request) {
	principal := h.caller(request)
	organization := h.organization(request)
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
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
	h.Logger.WarnContext(ctx, "operator issued a relay bootstrap token",
		slog.String("organization", organization.String()),
		slog.String("actor", principal.UserID().String()),
		slog.Time("expires_at", expiresAt),
		slog.String("caller", h.callerName(request)))

	writeJSON(writer, http.StatusCreated, bootstrapTokenView{
		Token:     token,
		ExpiresAt: expiresAt,
		Notice: "This token is shown once and cannot be read back. It enrols exactly one Relay " +
			"and is spent when it does; if it expires unused, issue another.",
	})
}

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
