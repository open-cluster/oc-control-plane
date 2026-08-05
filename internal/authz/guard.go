package authz

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// Refusals a credential resolver may report. Both answer 401 with the same body: telling a
// missing credential apart from a rejected one is how a caller learns which half of a guess
// was right, and the relay enrolment path refuses for the same reason.
var (
	// ErrNoCredential reports a request that presented nothing.
	ErrNoCredential = errors.New("no credential presented")
	// ErrCredentialRejected reports one that was presented and is not usable — unknown,
	// expired, revoked, or belonging to a disabled principal.
	ErrCredentialRejected = errors.New("credential rejected")
)

// Reason names WHY a credential did not authenticate, for the one audience that benefits: the
// operator holding it.
//
// A bare 401 is what leaves somebody reading a screen of error states and assuming the product
// is broken, which is what story 5 asks not to happen. It is safe to say — every value here
// means "you are not signed in", and none of them says anything about a session the caller
// does not already hold. It deliberately does NOT distinguish the ways a credential can be
// WRONG: a guess at somebody else's session is ReasonRejected, the same as a malformed one.
type Reason string

const (
	// ReasonRejected is the default and says nothing beyond the 401. Every unknown, malformed
	// or guessed credential is this.
	ReasonRejected Reason = "credential_rejected"
	// ReasonSessionExpired is a session that ran out on its own clock. The operator signs in
	// again and nothing is wrong.
	ReasonSessionExpired Reason = "session_expired"
	// ReasonSessionRevoked is a session an administrator ended, or one belonging to an account
	// that has been disabled. The operator signs in again and may find they cannot, which is
	// the outcome somebody intended and is worth telling them apart from a timeout.
	ReasonSessionRevoked Reason = "session_revoked"
)

// Refusal is an ErrCredentialRejected that says which. A resolver returns one where it knows;
// anything else is the bare rejection.
type Refusal struct{ Because Reason }

func (r Refusal) Error() string { return "credential rejected: " + string(r.Because) }

// Is makes a Refusal answer errors.Is(err, ErrCredentialRejected), so a caller that only wants
// to know the credential failed does not have to know this type exists.
func (r Refusal) Is(target error) bool { return target == ErrCredentialRejected }

// reasonOf reports what to tell the caller. Anything that is not a Refusal says nothing.
func reasonOf(err error) Reason {
	var refusal Refusal
	if errors.As(err, &refusal) && refusal.Because != "" {
		return refusal.Because
	}
	return ReasonRejected
}

// maxLoggedPath bounds what a refused request writes into the log. It runs before anything is
// authenticated, so the string is attacker-chosen and the repetition is unlimited.
const maxLoggedPath = 256

// Guard is the one place the authorization decision is made.
//
// Resolve turns whatever a request presents — a session cookie, a bearer token — into a
// principal. It is a function rather than an interface because the surface that assembles it
// owns which credentials it accepts, and this package must not learn that sessions exist.
//
// Record writes a refusal to the audit trail. It is best-effort by construction: a denial has
// no operation to roll back, and failing the response because the record could not be written
// would turn an unreachable database into a surface that answers 500 to callers it was
// correctly refusing anyway.
type Guard struct {
	Resolve func(*http.Request) (Principal, error)
	Record  func(context.Context, tenancy.Organization, audit.Event)
	// Origins are the browser origins a cookie-authenticated unsafe request may come from.
	// SameSite=Lax plus this check is the CSRF defence; there is no separate token, because a
	// token is a second thing to get wrong and Lax already refuses the cross-site form post.
	//
	// Empty means no browser may make an unsafe request, which is the correct posture for a
	// deployment that has not said where its console is served from.
	Origins []string
	Logger  *slog.Logger
}

// Router builds the operator mux from the route table. It is the only way a route reaches the
// surface: the table is validated here, so a route that cannot be authorized correctly is a
// failure to start rather than a route that is open.
func Router(table Table, guard Guard) (http.Handler, error) {
	if err := table.Validate(); err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	for _, route := range table {
		mux.Handle(route.Key(), guard.protect(route))
	}
	return mux, nil
}

// protect wraps one route in the decision its access declares.
func (g Guard) protect(route Route) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if route.access == AccessPublic {
			route.handler.ServeHTTP(writer, request)
			return
		}

		principal, err := g.Resolve(request)
		if err != nil || principal.IsZero() {
			g.refuseUnauthenticated(writer, request, err)
			return
		}

		// The CSRF check runs before the permission check so that a cross-site request is
		// refused for what it is, rather than being told which permission it was missing.
		if !g.originIsAllowed(principal, request) {
			g.refuseOrigin(writer, request, principal)
			return
		}

		if route.access == AccessAuthenticated {
			route.handler.ServeHTTP(writer, request.WithContext(
				WithPrincipal(request.Context(), principal)))
			return
		}

		organization, err := tenancy.NewOrganization(request.PathValue("organization"))
		if err != nil {
			writeJSON(writer, http.StatusBadRequest, errorView{Error: "organization is not a name"})
			return
		}

		role, member := principal.RoleIn(organization)
		if !member {
			// 404, not 403. A 403 confirms the tenant exists, which turns this surface into a
			// way to enumerate every customer the deployment has by editing a path segment.
			// The body is the same one an organization that does not exist produces, because
			// two answers that differ by a byte are two answers.
			g.recordRefusal(request, organization, principal, route, "not a member")
			writeJSON(writer, http.StatusNotFound, errorView{Error: "organization not found"})
			return
		}
		if !role.Grants(route.permission) {
			g.recordRefusal(request, organization, principal, route, "role does not grant it")
			writeJSON(writer, http.StatusForbidden, errorView{
				Error:    "forbidden",
				Requires: string(route.permission),
			})
			return
		}

		route.handler.ServeHTTP(writer, request.WithContext(
			WithPrincipal(request.Context(), principal)))
	})
}

// originIsAllowed answers the CSRF question for one request.
//
// It applies to cookie-borne credentials only. A browser attaches a cookie to a cross-site
// request without being asked and does not attach an Authorization header, so a bearer token
// is not forgeable this way and requiring an Origin from automation would break every client
// that is not a browser.
func (g Guard) originIsAllowed(principal Principal, request *http.Request) bool {
	if principal.Kind() != KindUser {
		return true
	}
	switch request.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}

	origin := strings.TrimSpace(request.Header.Get("Origin"))
	if origin == "" {
		// A browser sends Origin on every unsafe request, so its absence means the request did
		// not come from one — and a cookie-authenticated request that did not come from a
		// browser is not a case this surface serves.
		return false
	}
	for _, allowed := range g.Origins {
		if sameOrigin(origin, allowed) {
			return true
		}
	}
	return false
}

// sameOrigin compares two origins by scheme, host and port rather than by string, so a
// configured "https://console.example.com" matches a header that carries the same origin with
// a default port spelled out.
func sameOrigin(presented, allowed string) bool {
	first, err := url.Parse(presented)
	if err != nil {
		return false
	}
	second, err := url.Parse(strings.TrimSpace(allowed))
	if err != nil {
		return false
	}
	return strings.EqualFold(first.Scheme, second.Scheme) &&
		strings.EqualFold(first.Hostname(), second.Hostname()) &&
		portOf(first) == portOf(second)
}

func portOf(parsed *url.URL) string {
	if port := parsed.Port(); port != "" {
		return port
	}
	if strings.EqualFold(parsed.Scheme, "https") {
		return "443"
	}
	return "80"
}

// refuseUnauthenticated answers a request with no usable credential.
//
// Nothing the caller sent in a header is logged: a refused request's headers are the one place
// guaranteed to hold a guess at the credential. No audit event is written either, and that is
// a decision rather than an omission — an unauthenticated request names no organization to
// attribute an event to, and a table nothing may delete from is not somewhere an anonymous
// caller gets to write a row per request.
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

// recordRefusal writes the denial to the trail, so that credential probing by a party who does
// hold a credential is visible rather than only logged.
//
// A bound worth stating: a principal who holds any credential can produce one row per refused
// request here. The table is append-only and nothing prunes it in this slice, so the retention
// schedule is what keeps that finite. A rate limit on this path is the obvious next control
// and is deliberately not in this slice.
func (g Guard) recordRefusal(
	request *http.Request, organization tenancy.Organization,
	principal Principal, route Route, because string,
) {
	if g.Record == nil {
		return
	}
	g.Record(request.Context(), organization, audit.Event{
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
	})
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}

// errorView is the one shape this package answers with. It is deliberately the same shape the
// capability packages use, so a client parses one error body across the whole surface.
type errorView struct {
	Error string `json:"error"`
	// Reason says why a credential did not authenticate, so the interface can return an
	// operator to sign-in with an explanation rather than a screen of error states.
	Reason string `json:"reason,omitempty"`
	// Requires names the permission a member was missing. It is present only on a 403, where
	// the caller is already known to be a member and is being told what their role lacks.
	Requires string `json:"requires,omitempty"`
}

func writeJSON(writer http.ResponseWriter, status int, body any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(body)
}
