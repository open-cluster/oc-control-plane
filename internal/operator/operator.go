// Package operator serves the surface an operator uses to see and act on what the control
// plane knows about its relays.
//
// It exists because a detection nobody can query is not a detection. The control plane can
// tell that a relay identity is being taken over by two parties — the signature of a stolen
// credential — and until now that finding sat in a column with nothing to read it. The party
// who has to act on it is looking days later at a system that has since gone quiet.
//
// It is deliberately its own surface, on its own listener, for the same reason the relay
// endpoint is: it speaks to a different kind of caller and carries different data. Health and
// metrics are exposed to whatever scrapes them; this is not, and separating the ports is what
// lets a deployment bind it somewhere reachable only from inside.
//
// It is cross-tenant by design — an operator names the organization — and that is exactly why
// it is behind a credential of its own rather than the surface everything else uses.
package operator

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/connection"
	"github.com/open-cluster/oc-control-plane/internal/environment"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/storage"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// readTimeout bounds how long a read may take, so an operator query cannot outlive the
// attention of whoever made it.
const readTimeout = 15 * time.Second

// maxLoggedPath bounds what a refused request can write into the log. It runs before anything
// is authenticated, so the string is attacker-chosen and the repetition is unlimited.
const maxLoggedPath = 256

// Handlers is the operator surface's dependencies.
type Handlers struct {
	Placements *storage.Placements
	Logger     *slog.Logger
	// TokenDigest is the SHA-256 of the token a caller must present. Only the digest is held:
	// the process never keeps the token itself, so there is nothing here to log by accident.
	TokenDigest []byte
	// Controls and Versions are what an investigation opened through this surface runs under and
	// is stamped with. They are the composition root's to decide, not this package's: the
	// investigator's version is the binary's, and a control snapshot is pinned per round so that
	// "why did this round stop" survives the configuration that produced it.
	Controls investigation.Controls
	Versions investigation.Versions
}

// Router returns the operator surface.
//
// This package owns the listener and the credential; each business capability owns its own
// routes and the shape of what they return, and mounts them behind the authorization decided
// here. One surface deciding who may reach it is the point — a second copy of that decision
// is a second place for it to be wrong.
func (h Handlers) Router() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /operator/v1/organizations/{organization}/relays",
		h.authorized(http.HandlerFunc(h.listRelays)))
	mux.Handle("GET /operator/v1/organizations/{organization}/relays/{registration}/session-conflicts",
		h.authorized(http.HandlerFunc(h.conflictTrail)))
	mux.Handle("POST /operator/v1/organizations/{organization}/relays/{registration}/clear-conflict",
		h.authorized(http.HandlerFunc(h.clearConflict)))

	environment.Handlers{Placements: h.Placements, Logger: h.Logger}.Mount(mux, h.authorized)
	connection.Handlers{Placements: h.Placements, Logger: h.Logger}.Mount(mux, h.authorized)
	// The investigation surface takes the read side and the write side separately, because a
	// handler given the writing interface is one typo away from mutating what it was asked to
	// display. Both happen to be the same value here; the types are what keep them apart.
	investigation.Handlers{
		Reader:   h.Placements,
		Store:    h.Placements,
		Logger:   h.Logger,
		Controls: h.Controls,
		Versions: h.Versions,
	}.Mount(mux, h.authorized)
	return mux
}

// authorized refuses anything that does not present the operator token.
//
// One status and one message for every failure. A missing header, a malformed one and a wrong
// token are indistinguishable, for the same reason a relay's enrolment refusals are: telling
// them apart is how a caller learns which half of a guess was right. The comparison is
// constant-time over digests, so the answer leaks nothing through how long it took either.
func (h Handlers) authorized(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		presented, ok := bearerToken(request.Header.Get("Authorization"))
		if !ok {
			h.refuse(writer, request)
			return
		}
		digest := sha256.Sum256([]byte(presented))
		if subtle.ConstantTimeCompare(digest[:], h.TokenDigest) != 1 {
			h.refuse(writer, request)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func (h Handlers) refuse(writer http.ResponseWriter, request *http.Request) {
	// Nothing the caller sent in a header is recorded: a refused request's headers are the one
	// place guaranteed to hold a guess at the credential. The path is, because it says what was
	// being reached for — but truncated, because this runs before anything is authenticated and
	// an unbounded attacker-chosen string repeated without limit is a log amplifier.
	h.Logger.WarnContext(request.Context(), "operator request refused",
		slog.String("path", truncate(request.URL.Path, maxLoggedPath)),
		slog.String("caller", callerOf(request)))

	writer.Header().Set("WWW-Authenticate", "Bearer")
	writeJSON(writer, http.StatusUnauthorized, errorView{Error: "unauthorized"})
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}

// bearerToken pulls the credential out of an Authorization header.
func bearerToken(header string) (string, bool) {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	return token, token != ""
}

// listRelays reports an organization's relay identities and what is known about each.
func (h Handlers) listRelays(writer http.ResponseWriter, request *http.Request) {
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	roster, err := h.Placements.ListRelays(ctx, organization, storage.Page{
		Limit: pageSize(request),
		After: request.URL.Query().Get("after"),
	})
	if err != nil {
		h.fail(writer, request, err)
		return
	}

	// Every read of this surface crosses a tenant boundary, so every read is recorded. A token
	// holder who could enumerate an organization's relays and leave no trace would be the one
	// thing this surface must not make possible.
	h.Logger.InfoContext(ctx, "operator read a relay roster",
		slog.String("organization", organization.String()),
		slog.Int("relays", len(roster.Relays)),
		slog.String("caller", callerOf(request)))

	relays := make([]relayView, 0, len(roster.Relays))
	for _, relay := range roster.Relays {
		relays = append(relays, viewOf(relay))
	}
	writeJSON(writer, http.StatusOK, rosterView{Relays: relays, Next: roster.Next})
}

// conflictTrail reports what has happened to a relay identity.
//
// It is the answer to the question the current state cannot answer: withdrawing a finding
// destroys it, so without this the second occurrence would look exactly like the first.
func (h Handlers) conflictTrail(writer http.ResponseWriter, request *http.Request) {
	organization, registration, ok := h.relay(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	trail, err := h.Placements.SessionConflictTrail(ctx, organization, registration,
		storage.Page{Limit: pageSize(request), After: request.URL.Query().Get("after")})
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	h.Logger.InfoContext(ctx, "operator read a session conflict trail",
		slog.String("organization", organization.String()),
		slog.String("registration_id", registration.String()),
		slog.String("caller", callerOf(request)))

	events := make([]conflictEventView, 0, len(trail.Events))
	for _, event := range trail.Events {
		events = append(events, eventViewOf(event))
	}
	writeJSON(writer, http.StatusOK, trailView{Events: events, Next: trail.Next})
}

// clearConflict withdraws the mark on a contested relay identity.
func (h Handlers) clearConflict(writer http.ResponseWriter, request *http.Request) {
	organization, registration, ok := h.relay(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	withdrawal, err := h.Placements.ClearSessionConflict(
		ctx, organization, registration, callerOf(request))
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	switch withdrawal {
	case storage.WithdrawalRelayUnknown:
		writeJSON(writer, http.StatusNotFound, errorView{Error: "relay not found"})
		return
	case storage.WithdrawalNothingMarked:
		// The state asked for already holds. Nothing was written to the trail, because an act
		// that changed nothing is not part of the history of what happened.
		writer.WriteHeader(http.StatusNoContent)
		return
	}
	// Withdrawing the mark is a claim that a credential-theft finding has been dealt with, and
	// it destroys the finding, so it is recorded as loudly as the finding was.
	//
	// The caller's address is as far as attribution goes. The credential is one shared token, so
	// this line can say where the claim came from and never who made it — which is a limit of
	// having one token rather than of this record, and is worth knowing when reading it back.
	h.Logger.WarnContext(ctx, "session conflict cleared by an operator",
		slog.String("organization", organization.String()),
		slog.String("registration_id", registration.String()),
		slog.String("caller", callerOf(request)))

	writer.WriteHeader(http.StatusNoContent)
}

// callerOf reports where a request came from. It is the only identity this surface has: one
// shared token cannot say who, only from where.
func callerOf(request *http.Request) string {
	return request.RemoteAddr
}

// organization resolves the tenant named in the path.
func (h Handlers) organization(
	writer http.ResponseWriter, request *http.Request,
) (tenancy.Organization, bool) {
	organization, err := tenancy.NewOrganization(request.PathValue("organization"))
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "organization is not a name"})
		return tenancy.Organization{}, false
	}
	return organization, true
}

// relay resolves the tenant and the relay named in the path, for the routes that address one.
func (h Handlers) relay(
	writer http.ResponseWriter, request *http.Request,
) (tenancy.Organization, uuid.UUID, bool) {
	organization, ok := h.organization(writer, request)
	if !ok {
		return tenancy.Organization{}, uuid.UUID{}, false
	}
	registration, err := uuid.Parse(request.PathValue("registration"))
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "registration is not an identity"})
		return tenancy.Organization{}, uuid.UUID{}, false
	}
	return organization, registration, true
}

// fail answers an error, naming the ones a caller can act on. The caller is an operator, so
// saying which it was costs nothing and saves them guessing.
//
// An organization this instance has no placement for is reported as not served. Note that a
// deployment with a default placement serves every name, so there this answer never appears and
// an unknown organization is an empty list — which is the placement model showing through
// rather than this surface being evasive.
func (h Handlers) fail(writer http.ResponseWriter, request *http.Request, err error) {
	if errors.Is(err, storage.ErrUnknownOrganization) {
		writeJSON(writer, http.StatusNotFound, errorView{Error: "organization not served here"})
		return
	}
	if errors.Is(err, storage.ErrBadCursor) {
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: "after is not a page position from a previous response"})
		return
	}
	h.Logger.ErrorContext(request.Context(), "operator request failed",
		slog.String("path", request.URL.Path),
		slog.String("error", err.Error()))
	writeJSON(writer, http.StatusInternalServerError, errorView{Error: "request failed"})
}

// pageSize reads how many relays were asked for. An unreadable value is not an error: the
// bound is the point, and storage clamps whatever arrives.
func pageSize(request *http.Request) int {
	size, err := strconv.Atoi(request.URL.Query().Get("limit"))
	if err != nil {
		return 0
	}
	return size
}
