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
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/storage"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// readTimeout bounds how long a read may take, so an operator query cannot outlive the
// attention of whoever made it.
const readTimeout = 15 * time.Second

// Handlers is the operator surface's dependencies.
type Handlers struct {
	Placements *storage.Placements
	Logger     *slog.Logger
	// TokenDigest is the SHA-256 of the token a caller must present. Only the digest is held:
	// the process never keeps the token itself, so there is nothing here to log by accident.
	TokenDigest []byte
}

// Router returns the operator surface.
func (h Handlers) Router() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /operator/v1/organizations/{organization}/relays",
		h.authorized(http.HandlerFunc(h.listRelays)))
	mux.Handle("POST /operator/v1/organizations/{organization}/relays/{registration}/clear-conflict",
		h.authorized(http.HandlerFunc(h.clearConflict)))
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
	// The path is recorded and nothing the caller sent is. A refused request's own headers are
	// the one thing guaranteed to contain a guess at the credential.
	h.Logger.WarnContext(request.Context(), "operator request refused",
		slog.String("path", request.URL.Path))

	writer.Header().Set("WWW-Authenticate", "Bearer")
	writeJSON(writer, http.StatusUnauthorized, errorView{Error: "unauthorized"})
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
	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	roster, err := h.Placements.ListRelays(ctx, organization, pageSize(request))
	if err != nil {
		h.fail(writer, request, err)
		return
	}

	relays := make([]relayView, 0, len(roster.Relays))
	for _, relay := range roster.Relays {
		relays = append(relays, viewOf(relay))
	}
	writeJSON(writer, http.StatusOK, rosterView{Relays: relays, More: roster.More})
}

// clearConflict withdraws the mark on a contested relay identity.
func (h Handlers) clearConflict(writer http.ResponseWriter, request *http.Request) {
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	registration, err := uuid.Parse(request.PathValue("registration"))
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "registration is not an identity"})
		return
	}
	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	cleared, err := h.Placements.ClearSessionConflict(ctx, organization, registration)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	if !cleared {
		writeJSON(writer, http.StatusNotFound, errorView{Error: "relay not found"})
		return
	}
	// Withdrawing the mark is itself worth recording: it is a person saying the thing that was
	// detected has been dealt with, and that claim should be as findable as the detection.
	h.Logger.InfoContext(ctx, "session conflict cleared by an operator",
		slog.String("organization", organization.String()),
		slog.String("registration_id", registration.String()))

	writer.WriteHeader(http.StatusNoContent)
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

// fail answers an error, distinguishing an organization this deployment does not serve from
// something having gone wrong. The caller is an operator, so naming which it was costs nothing
// and saves them guessing.
func (h Handlers) fail(writer http.ResponseWriter, request *http.Request, err error) {
	if errors.Is(err, storage.ErrUnknownOrganization) {
		writeJSON(writer, http.StatusNotFound, errorView{Error: "organization not served here"})
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
