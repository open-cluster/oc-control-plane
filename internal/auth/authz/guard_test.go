package authz_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
)

const (
	relaysPattern      = "/api/v1/organizations/{organization}/relays"
	clearPattern       = "/api/v1/organizations/{organization}/relays/{registration}/clear-conflict"
	sessionPattern     = "/api/v1/session"
	organizationHeader = "X-OpenCluster-Organization"
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
		ResolveOrganization: func(context.Context, tenancy.Organization) (bool, error) {
			return true, nil
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

	router := guardOver(t, memberOf(t, "org-a", authz.Admin), nil)

	existsElsewhere, foreignBody := call(t, router, http.MethodGet,
		"/api/v1/organizations/org-b/relays", organizationHeader, "org-b")
	invented, inventedBody := call(t, router, http.MethodGet,
		"/api/v1/organizations/org-nobody-has/relays",
		organizationHeader, "org-nobody-has")

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

func TestAnOrganizationScopedRouteRequiresAnActiveOrganizationSelector(t *testing.T) {
	t.Parallel()

	status, _ := call(t, guardOver(t, memberOf(t, "org-a", authz.Admin), nil),
		http.MethodGet, "/api/v1/organizations/org-a/relays")

	if status != http.StatusBadRequest {
		t.Errorf("a request without %s answered %d, want 400", organizationHeader, status)
	}
}

func TestTheActiveOrganizationSelectorCannotConflictWithTheRoute(t *testing.T) {
	t.Parallel()

	status, _ := call(t, guardOver(t, memberOf(t, "org-a", authz.Admin), nil),
		http.MethodGet, "/api/v1/organizations/org-a/relays",
		organizationHeader, "org-b")

	if status != http.StatusBadRequest {
		t.Errorf("a selector conflicting with the route answered %d, want 400", status)
	}
}

func TestTheVerifiedActiveOrganizationReachesTheHandler(t *testing.T) {
	t.Parallel()

	var observed string
	router, err := authz.Router(authz.Table{
		authz.Privileged(http.MethodGet, relaysPattern, authz.RelayRead,
			http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
				organization, ok := authz.ActiveOrganizationFrom(request.Context())
				if ok {
					observed = organization.String()
				}
			})),
	}, authz.Guard{
		Resolve: func(*http.Request) (authz.Principal, error) {
			return memberOf(t, "org-a", authz.Admin), nil
		},
		ResolveOrganization: func(context.Context, tenancy.Organization) (bool, error) {
			return true, nil
		},
		Origins: []string{"https://console.example.com"},
		Logger:  quietLogger(),
	})
	if err != nil {
		t.Fatalf("building the router: %v", err)
	}

	status, _ := call(t, router, http.MethodGet,
		"/api/v1/organizations/org-a/relays", organizationHeader, "org-a")
	if status != http.StatusOK {
		t.Fatalf("a valid request answered %d, want 200", status)
	}
	if observed != "org-a" {
		t.Errorf("handler observed active organization %q, want org-a", observed)
	}
}

func TestTheActiveOrganizationIsResolvedBeforeTheHandler(t *testing.T) {
	t.Parallel()

	handled := false
	router, err := authz.Router(authz.Table{
		authz.Privileged(http.MethodGet, relaysPattern, authz.RelayRead,
			http.HandlerFunc(func(http.ResponseWriter, *http.Request) { handled = true })),
	}, authz.Guard{
		Resolve: func(*http.Request) (authz.Principal, error) {
			return memberOf(t, "org-a", authz.Admin), nil
		},
		ResolveOrganization: func(context.Context, tenancy.Organization) (bool, error) {
			return false, nil
		},
		Logger: quietLogger(),
	})
	if err != nil {
		t.Fatalf("building the router: %v", err)
	}

	status, body := call(t, router, http.MethodGet,
		"/api/v1/organizations/org-a/relays", organizationHeader, "org-a")
	if status != http.StatusNotFound || body != "{\"error\":\"organization not found\"}\n" {
		t.Fatalf("an unresolved Organization answered %d %q, want indistinguishable 404", status, body)
	}
	if handled {
		t.Error("the handler ran for an unresolved Organization")
	}
}

func TestMembershipIsVerifiedBeforeCSRF(t *testing.T) {
	t.Parallel()

	status, body := call(t, guardOver(t, memberOf(t, "org-a", authz.Admin), nil),
		http.MethodPost, "/api/v1/organizations/org-b/relays/r1/clear-conflict",
		organizationHeader, "org-b", "Origin", "https://evil.example.com")

	if status != http.StatusNotFound {
		t.Fatalf("an inaccessible organization with a bad origin answered %d, want 404", status)
	}
	if body != "{\"error\":\"organization not found\"}\n" {
		t.Errorf("inaccessible organization response = %q", body)
	}
}

func TestAnOriginContainingCredentialsIsRefused(t *testing.T) {
	t.Parallel()

	status, _ := call(t, guardOver(t, memberOf(t, "org-a", authz.Admin), nil),
		http.MethodPost, "/api/v1/organizations/org-a/relays/r1/clear-conflict",
		organizationHeader, "org-a", "Origin", "https://attacker@console.example.com")

	if status != http.StatusForbidden {
		t.Errorf("an origin containing credentials answered %d, want 403", status)
	}
}

func TestTheActiveOrganizationSelectorMustBeOneValidValue(t *testing.T) {
	t.Parallel()

	router := guardOver(t, memberOf(t, "org-a", authz.Admin), nil)
	for _, testCase := range []struct {
		name   string
		values []string
	}{
		{name: "repeated", values: []string{"org-a", "org-a"}},
		{name: "malformed", values: []string{"org a"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			request := httptest.NewRequest(http.MethodGet,
				"/api/v1/organizations/org-a/relays", nil)
			for _, value := range testCase.values {
				request.Header.Add(organizationHeader, value)
			}
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusBadRequest {
				t.Errorf("%s selector answered %d, want 400", testCase.name, recorder.Code)
			}
		})
	}
}

func TestABodyOrganizationCannotConflictBeforeTheHandler(t *testing.T) {
	t.Parallel()

	handled := false
	router, err := authz.Router(authz.Table{
		authz.Privileged(http.MethodPost, clearPattern, authz.RelayConflictClear,
			http.HandlerFunc(func(http.ResponseWriter, *http.Request) { handled = true })),
	}, authz.Guard{
		Resolve: func(*http.Request) (authz.Principal, error) {
			return memberOf(t, "org-a", authz.Admin), nil
		},
		ResolveOrganization: func(context.Context, tenancy.Organization) (bool, error) {
			return true, nil
		},
		Origins: []string{"https://console.example.com"},
		Logger:  quietLogger(),
	})
	if err != nil {
		t.Fatalf("building the router: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost,
		"/api/v1/organizations/org-a/relays/r1/clear-conflict",
		strings.NewReader(`{"organization":"org-b"}`))
	request.Header.Set(organizationHeader, "org-a")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://console.example.com")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Errorf("a body Organization conflicting with the selector answered %d, want 400",
			recorder.Code)
	}
	if handled {
		t.Error("the handler ran with a conflicting body Organization")
	}
}

func TestTheOrganizationCheckPreservesAnOrdinaryJSONBody(t *testing.T) {
	t.Parallel()

	const body = `{"name":"relay-a"}`
	var observed string
	router, err := authz.Router(authz.Table{
		authz.Privileged(http.MethodPost, clearPattern, authz.RelayConflictClear,
			http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
				read, _ := io.ReadAll(request.Body)
				observed = string(read)
			})),
	}, authz.Guard{
		Resolve: func(*http.Request) (authz.Principal, error) {
			return memberOf(t, "org-a", authz.Admin), nil
		},
		ResolveOrganization: func(context.Context, tenancy.Organization) (bool, error) {
			return true, nil
		},
		Origins: []string{"https://console.example.com"},
		Logger:  quietLogger(),
	})
	if err != nil {
		t.Fatalf("building the router: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost,
		"/api/v1/organizations/org-a/relays/r1/clear-conflict", strings.NewReader(body))
	request.Header.Set(organizationHeader, "org-a")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://console.example.com")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK || observed != body {
		t.Errorf("ordinary body reached handler as %q with status %d", observed, recorder.Code)
	}
}

func TestAnAuthenticationRefusalDoesNotLogCredentialHeaders(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	router, err := authz.Router(authz.Table{
		authz.Authenticated(http.MethodGet, sessionPattern, http.HandlerFunc(served)),
	}, authz.Guard{
		Resolve: func(*http.Request) (authz.Principal, error) {
			return authz.Principal{}, authz.ErrCredentialRejected
		},
		Logger: logger,
	})
	if err != nil {
		t.Fatalf("building the router: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, sessionPattern, nil)
	request.Header.Set("Authorization", "Bearer never-log-this-token")
	request.Header.Set("Cookie", "session=never-log-this-cookie")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	for _, secret := range []string{"never-log-this-token", "never-log-this-cookie"} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("authentication refusal logged credential %q: %s", secret, logs.String())
		}
	}
}

// A member who lacks the permission gets 403, not 404. They already know the tenant exists —
// they are in it — so hiding it costs them the ability to tell "I may not" from "it is gone".
func TestAMemberWithoutThePermissionIsToldWhatTheyLack(t *testing.T) {
	t.Parallel()

	router := guardOver(t, memberOf(t, "org-a", authz.Viewer), nil)

	status, body := call(t, router, http.MethodPost,
		"/api/v1/organizations/org-a/relays/r1/clear-conflict",
		"Origin", "https://console.example.com", organizationHeader, "org-a")

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

	call(t, router, http.MethodPost, "/api/v1/organizations/org-a/relays/r1/clear-conflict",
		"Origin", "https://console.example.com", organizationHeader, "org-a")
	call(t, router, http.MethodGet, "/api/v1/organizations/org-b/relays",
		organizationHeader, "org-b")
	call(t, router, http.MethodPost, "/api/v1/organizations/org-a/relays/r1/clear-conflict",
		organizationHeader, "org-a", "Origin", "https://evil.example.com")
	call(t, router, http.MethodGet, "/api/v1/organizations/org-b/relays",
		organizationHeader, "org-a")

	if len(recorded) != 4 {
		t.Fatalf("the trail holds %d refusals, want permission, membership, CSRF, and selector misses",
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

	status, _ := call(t, router, http.MethodGet, "/api/v1/organizations/org-a/relays")
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

	router := guardOver(t, memberOf(t, "org-a", authz.Admin), nil)
	const path = "/api/v1/organizations/org-a/relays/r1/clear-conflict"

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
			headers = append(headers, organizationHeader, "org-a")
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
			authz.Privileged(http.MethodGet, "/api/v1/relays", authz.RelayRead,
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
		authz.Public(http.MethodGet, "/api/v1/sign-in/callback", http.HandlerFunc(served)),
	}
	if err := valid.Validate(); err != nil {
		t.Errorf("a correct table was refused: %v", err)
	}
}

func TestTheRouterRequiresAnOrganizationResolverForPrivilegedRoutes(t *testing.T) {
	t.Parallel()

	_, err := authz.Router(authz.Table{
		authz.Privileged(http.MethodGet, relaysPattern, authz.RelayRead,
			http.HandlerFunc(served)),
	}, authz.Guard{Resolve: func(*http.Request) (authz.Principal, error) {
		return memberOf(t, "org-a", authz.Admin), nil
	}, Logger: quietLogger()})
	if err == nil {
		t.Error("a privileged router with no Organization resolver was accepted")
	}
}
