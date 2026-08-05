package authz_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

const (
	relaysPattern  = "/operator/v1/organizations/{organization}/relays"
	clearPattern   = "/operator/v1/organizations/{organization}/relays/{registration}/clear-conflict"
	sessionPattern = "/operator/v1/session"
)

func served(http.ResponseWriter, *http.Request) {}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func organizationNamed(t *testing.T, name string) tenancy.Organization {
	t.Helper()
	organization, err := tenancy.NewOrganization(name)
	if err != nil {
		t.Fatalf("naming %q: %v", name, err)
	}
	return organization
}

func memberOf(t *testing.T, name string, role authz.Role) authz.Principal {
	t.Helper()
	principal, err := authz.NewPrincipal(authz.KindUser, "user-1", "Ada", []authz.Membership{
		{Organization: organizationNamed(t, name), Role: role},
	})
	if err != nil {
		t.Fatalf("building a principal: %v", err)
	}
	return principal
}

// guardOver builds a surface serving the three routes below as whoever the test says is
// calling. Everything the guard decides is observable from the response, which is the only
// thing a caller can see.
func guardOver(t *testing.T, principal authz.Principal, recorded *[]audit.Event) http.Handler {
	t.Helper()

	guard := authz.Guard{
		Resolve: func(*http.Request) (authz.Principal, error) {
			if principal.IsZero() {
				return authz.Principal{}, authz.ErrNoCredential
			}
			return principal, nil
		},
		Record: func(_ context.Context, _ tenancy.Organization, event audit.Event) {
			if recorded != nil {
				*recorded = append(*recorded, event)
			}
		},
		Origins: []string{"https://console.example.com"},
		Logger:  quietLogger(),
	}

	router, err := authz.Router(authz.Table{
		authz.Privileged(http.MethodGet, relaysPattern, authz.RelayRead,
			http.HandlerFunc(served)),
		authz.Privileged(http.MethodPost, clearPattern, authz.RelayConflictClear,
			http.HandlerFunc(served)),
		authz.Authenticated(http.MethodGet, sessionPattern, http.HandlerFunc(served)),
	}, guard)
	if err != nil {
		t.Fatalf("building the router: %v", err)
	}
	return router
}

func call(t *testing.T, router http.Handler, method, path string, headers ...string) (int, string) {
	t.Helper()

	request := httptest.NewRequest(method, path, nil)
	for index := 0; index+1 < len(headers); index += 2 {
		request.Header.Set(headers[index], headers[index+1])
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder.Code, recorder.Body.String()
}

// Story 24, and the property the whole tenancy check exists for: a request naming an
// Organization the caller does not belong to must be indistinguishable from one naming an
// Organization that does not exist. A 403 would confirm the tenant exists, which turns a path
// segment into a customer list.
func TestAForeignOrganizationIsIndistinguishableFromOneThatDoesNotExist(t *testing.T) {
	t.Parallel()

	router := guardOver(t, memberOf(t, "org-a", authz.OrganizationOwner), nil)

	existsElsewhere, foreignBody := call(t, router, http.MethodGet,
		"/operator/v1/organizations/org-b/relays")
	invented, inventedBody := call(t, router, http.MethodGet,
		"/operator/v1/organizations/org-nobody-has/relays")

	if existsElsewhere != http.StatusNotFound {
		t.Errorf("another tenant's organization answered %d, want 404; a 403 confirms it exists",
			existsElsewhere)
	}
	if invented != http.StatusNotFound {
		t.Errorf("an organization nobody has answered %d, want 404", invented)
	}
	if foreignBody != inventedBody {
		t.Errorf("the two answers differ:\n  foreign: %q\n  invented: %q\nA difference of one "+
			"byte is a difference", foreignBody, inventedBody)
	}
}

// A member who lacks the permission gets 403, not 404. They already know the tenant exists —
// they are in it — so hiding it costs them the ability to tell "I may not" from "it is gone".
func TestAMemberWithoutThePermissionIsToldWhatTheyLack(t *testing.T) {
	t.Parallel()

	router := guardOver(t, memberOf(t, "org-a", authz.Viewer), nil)

	status, body := call(t, router, http.MethodPost,
		"/operator/v1/organizations/org-a/relays/r1/clear-conflict",
		"Origin", "https://console.example.com")

	if status != http.StatusForbidden {
		t.Fatalf("a viewer clearing a conflict answered %d, want 403", status)
	}
	if !strings.Contains(body, string(authz.RelayConflictClear)) {
		t.Errorf("the refusal does not name what was missing: %s", body)
	}
}

// Story 22: a refusal by someone holding a credential is on the record, because credential
// probing is only visible if the attempts that failed are.
func TestARefusedAuthorizationIsRecorded(t *testing.T) {
	t.Parallel()

	var recorded []audit.Event
	router := guardOver(t, memberOf(t, "org-a", authz.Viewer), &recorded)

	call(t, router, http.MethodPost, "/operator/v1/organizations/org-a/relays/r1/clear-conflict",
		"Origin", "https://console.example.com")
	call(t, router, http.MethodGet, "/operator/v1/organizations/org-b/relays")

	if len(recorded) != 2 {
		t.Fatalf("the trail holds %d refusals, want the permission miss and the membership miss",
			len(recorded))
	}
	for _, event := range recorded {
		if event.Outcome != audit.OutcomeDenied {
			t.Errorf("a refusal was recorded as %s", event.Outcome)
		}
		if event.Actor.ID != "user-1" {
			t.Errorf("a refusal names the actor %q", event.Actor.ID)
		}
		if event.Action != audit.ActionAuthorizationRefused {
			t.Errorf("a refusal was recorded as %q", event.Action)
		}
	}
}

// An unauthenticated request is refused before anything else and writes no event: it names no
// organization to attribute one to, and the table nothing may delete from is not somewhere an
// anonymous caller writes a row per request.
func TestAnUnauthenticatedRequestIsRefusedAndNotRecorded(t *testing.T) {
	t.Parallel()

	var recorded []audit.Event
	router := guardOver(t, authz.Principal{}, &recorded)

	status, _ := call(t, router, http.MethodGet, "/operator/v1/organizations/org-a/relays")
	if status != http.StatusUnauthorized {
		t.Errorf("a request with no credential answered %d, want 401", status)
	}
	if len(recorded) != 0 {
		t.Errorf("an anonymous refusal wrote %d audit rows", len(recorded))
	}
}

// SameSite=Lax plus this check is the whole CSRF defence. A cookie-authenticated unsafe request
// from anywhere but the console is refused, and one with no Origin at all did not come from a
// browser — which is not a case a cookie-authenticated route serves.
func TestACookieBorneUnsafeRequestNeedsAnAllowedOrigin(t *testing.T) {
	t.Parallel()

	router := guardOver(t, memberOf(t, "org-a", authz.OrganizationOwner), nil)
	const path = "/operator/v1/organizations/org-a/relays/r1/clear-conflict"

	for _, testCase := range []struct {
		name   string
		origin string
		want   int
	}{
		{"the console", "https://console.example.com", http.StatusNoContent},
		{"somewhere else", "https://evil.example.com", http.StatusForbidden},
		{"nowhere at all", "", http.StatusForbidden},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			var headers []string
			if testCase.origin != "" {
				headers = []string{"Origin", testCase.origin}
			}
			status, _ := call(t, router, http.MethodPost, path, headers...)

			// The handler under test writes nothing, so a request that reached it is 200.
			passed := status == http.StatusOK
			wanted := testCase.want == http.StatusNoContent
			if passed != wanted {
				t.Errorf("origin %q answered %d; reaching the handler should be %v",
					testCase.origin, status, wanted)
			}
		})
	}
}

// Automation presents a bearer token, which a browser never attaches by itself, so requiring an
// Origin from it would break every client that is not a browser for no gain.
func TestAServiceAccountNeedsNoOrigin(t *testing.T) {
	t.Parallel()

	principal, err := authz.NewPrincipal(authz.KindServiceAccount, "svc-1", "ci",
		[]authz.Membership{{
			Organization: organizationNamed(t, "org-a"), Role: authz.PlatformAdministrator,
		}})
	if err != nil {
		t.Fatalf("building a service principal: %v", err)
	}

	status, body := call(t, guardOver(t, principal, nil), http.MethodPost,
		"/operator/v1/organizations/org-a/relays/r1/clear-conflict")

	if status != http.StatusOK {
		t.Errorf("a token-bearing caller answered %d %s, want the handler to have run",
			status, body)
	}
}

// A route needing only a credential must not need a membership, or an auditor could not sign
// out and a user with no memberships yet could not be told they have none.
func TestAnAuthenticatedRouteNeedsNoMembership(t *testing.T) {
	t.Parallel()

	principal, err := authz.NewPrincipal(authz.KindUser, "user-1", "Ada", nil)
	if err != nil {
		t.Fatalf("building a principal: %v", err)
	}

	if status, _ := call(t, guardOver(t, principal, nil), http.MethodGet, sessionPattern); status != http.StatusOK {
		t.Errorf("reading one's own session answered %d, want it served", status)
	}
}

// The table is what makes "a route without a declared permission cannot ship" true, so the
// validation is the load-bearing part and is asserted in both directions.
func TestTheRouteTableRefusesWhatCannotBeAuthorized(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name  string
		table authz.Table
	}{
		{"a permission this build does not declare", authz.Table{
			authz.Privileged(http.MethodGet, relaysPattern, "relay.invented",
				http.HandlerFunc(served)),
		}},
		{"a privileged route naming no organization", authz.Table{
			authz.Privileged(http.MethodGet, "/operator/v1/relays", authz.RelayRead,
				http.HandlerFunc(served)),
		}},
		{"the same route twice", authz.Table{
			authz.Privileged(http.MethodGet, relaysPattern, authz.RelayRead,
				http.HandlerFunc(served)),
			authz.Privileged(http.MethodGet, relaysPattern, authz.RelayRead,
				http.HandlerFunc(served)),
		}},
		{"no routes at all", authz.Table{}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if err := testCase.table.Validate(); err == nil {
				t.Error("the table was accepted; a route that cannot be authorized correctly " +
					"must be a failure to start rather than a route that is open")
			}
		})
	}

	valid := authz.Table{
		authz.Privileged(http.MethodGet, relaysPattern, authz.RelayRead, http.HandlerFunc(served)),
		authz.Authenticated(http.MethodGet, sessionPattern, http.HandlerFunc(served)),
		authz.Public(http.MethodGet, "/operator/v1/sign-in/callback", http.HandlerFunc(served)),
	}
	if err := valid.Validate(); err != nil {
		t.Errorf("a correct table was refused: %v", err)
	}
}
