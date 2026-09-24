package identity

import (
	"errors"
	"net/http"

	"github.com/open-cluster/oc-control-plane/internal/api/listing"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/session"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

// session answers who is signed in and describes their current Organization and Role.
func (h Handlers) session(writer http.ResponseWriter, request *http.Request) {
	principal := h.caller(request)

	writeJSON(writer, http.StatusOK, sessionViewOf(principal))
}

func (h Handlers) signOut(writer http.ResponseWriter, request *http.Request, principal authz.Principal) {
	if principal.IsZero() {
		writeJSON(writer, http.StatusOK, signOutView{SignedOut: true})
		return
	}
	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	if err := h.Database.RevokeCurrentSession(ctx, principal, principal.SessionID()); err != nil && !errors.Is(err, session.ErrUnknown) {
		h.fail(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, signOutView{SignedOut: true})
}

func (h Handlers) listSessions(writer http.ResponseWriter, request *http.Request) {
	query, ok := listQuery(writer, request, listing.Spec{
		DefaultSort: listing.Sort{Field: "lastSeenAt", Descending: true},
	})
	if !ok {
		return
	}
	principal := h.caller(request)
	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	live, err := h.Database.ListSessions(ctx, principal, storage.Page{
		Limit: query.Limit, After: query.Cursor,
	})
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	views := make([]liveSessionView, 0, len(live.Sessions))
	for _, found := range live.Sessions {
		views = append(views, liveSessionViewOf(found))
	}
	writeJSON(writer, http.StatusOK, liveSessionListView{
		Sessions: views, Next: listing.CursorPtr(live.Next),
	})
}

func (h Handlers) revokeSession(writer http.ResponseWriter, request *http.Request) {
	principal := h.caller(request)
	sessionID, ok := identifier(writer, request, "session")
	if !ok {
		return
	}
	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()
	if err := h.Database.RevokeSession(ctx, principal, sessionID); err != nil {
		h.fail(writer, request, err)
		return
	}
	if principal.SessionID() == sessionID {
		session.Clear(writer)
	}
	writer.WriteHeader(http.StatusNoContent)
}
