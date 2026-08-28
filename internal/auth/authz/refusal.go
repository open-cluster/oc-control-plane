package authz

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
)

var (
	// ErrNoCredential reports a request that presented no supported credential.
	ErrNoCredential = errors.New("no credential presented")
	// ErrCredentialRejected reports a presented credential that cannot authenticate.
	ErrCredentialRejected = errors.New("credential rejected")
)

// Reason is the safe client-facing category for an authentication refusal.
type Reason string

const (
	// ReasonRejected reveals no detail about why a presented credential was unusable.
	ReasonRejected Reason = "credential_rejected"
	// ReasonSessionExpired tells the holder of a known session to sign in again.
	ReasonSessionExpired Reason = "session_expired"
	// ReasonSessionRevoked tells the holder that the known session was ended deliberately.
	ReasonSessionRevoked Reason = "session_revoked"
)

// Refusal is an ErrCredentialRejected with a safe client-facing reason.
type Refusal struct{ Because Reason }

func (r Refusal) Error() string        { return "credential rejected: " + string(r.Because) }
func (r Refusal) Is(target error) bool { return target == ErrCredentialRejected }

func reasonOf(err error) Reason {
	var refusal Refusal
	if errors.As(err, &refusal) && refusal.Because != "" {
		return refusal.Because
	}
	return ReasonRejected
}

const maxLoggedPath = 256

func (g Guard) refuseUnauthenticated(
	writer http.ResponseWriter, request *http.Request, because error,
) {
	reason := ReasonRejected
	if !errors.Is(because, ErrNoCredential) {
		reason = reasonOf(because)
	}
	g.Logger.WarnContext(request.Context(), "operator request refused",
		slog.String("path", truncate(request.URL.Path, maxLoggedPath)),
		slog.String("reason", string(reason)),
		slog.String("caller", request.RemoteAddr))
	writer.Header().Set("WWW-Authenticate", "Bearer")
	writeJSON(writer, http.StatusUnauthorized,
		errorView{Error: "unauthorized", Reason: string(reason)})
}

func (g Guard) refuseOrigin(
	writer http.ResponseWriter, request *http.Request, principal Principal,
) {
	g.Logger.WarnContext(request.Context(), "operator request refused: origin",
		slog.String("path", truncate(request.URL.Path, maxLoggedPath)),
		slog.String("actor", principal.ID()),
		slog.String("caller", request.RemoteAddr))
	writeJSON(writer, http.StatusForbidden, errorView{Error: "origin not allowed"})
}

func (g Guard) recordRefusal(
	request *http.Request, organization tenancy.Organization,
	principal Principal, route Route, because string,
) {
	if g.Record == nil {
		return
	}
	event := audit.Event{
		Organization:  organization.String(),
		Actor:         principal.Actor(),
		Action:        audit.ActionAuthorizationRefused,
		Target:        audit.Target{Kind: audit.TargetRoute, ID: route.Key()},
		Outcome:       audit.OutcomeDenied,
		SourceAddress: request.RemoteAddr,
		RequestID:     principal.RequestID(),
		Detail: audit.Detail{
			"reason":   because,
			"requires": string(route.permission),
		},
	}
	g.Record(request.Context(), organization, event)
}

func (g Guard) recordAttributableRefusal(
	request *http.Request, organization tenancy.Organization,
	principal Principal, route Route, because string,
) {
	if organization.IsEmpty() || !principal.MemberOf(organization) ||
		g.ResolveOrganization == nil {
		return
	}
	known, err := g.ResolveOrganization(request.Context(), organization)
	if err != nil || !known {
		return
	}
	g.recordRefusal(request, organization, principal, route, because)
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}

type errorView struct {
	Error    string `json:"error"`
	Reason   string `json:"reason,omitempty"`
	Requires string `json:"requires,omitempty"`
}

func writeJSON(writer http.ResponseWriter, status int, body any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(body)
}
