package authz_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
)

const organizationID = "11111111-1111-4111-8111-111111111111"

func principal(t *testing.T, role authz.Role) authz.Principal {
	t.Helper()
	organization := uuid.MustParse(organizationID)
	principal, err := authz.NewPrincipal(uuid.New(), uuid.New(), "Ada", authz.Membership{
		Organization: organization, DisplayName: "Operations", Role: role,
	})
	if err != nil {
		t.Fatal(err)
	}
	return principal
}

func router(t *testing.T, resolved authz.Principal, permission authz.Permission, recorded *[]audit.Event) http.Handler {
	t.Helper()
	routes := []authz.Route{{
		Method: http.MethodPost, Pattern: "/api/v1/action", Permission: permission,
		Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			found := authz.MustPrincipal(request.Context())
			if found.Organization().String() != organizationID {
				t.Fatalf("handler Organization = %s", found.Organization())
			}
			writer.WriteHeader(http.StatusNoContent)
		}),
	}}
	guard := authz.Guard{
		Resolve: func(*http.Request) (authz.Principal, error) {
			if resolved.IsZero() {
				return authz.Principal{}, authz.ErrNoCredential
			}
			return resolved, nil
		},
		Origin: "https://console.example.com",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if recorded != nil {
		guard.Record = func(_ context.Context, _ uuid.UUID, event audit.Event) {
			*recorded = append(*recorded, event)
		}
	}
	built, err := authz.Router(routes, guard)
	if err != nil {
		t.Fatal(err)
	}
	return built
}

func request(handler http.Handler, origin string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	wanted := httptest.NewRequest(http.MethodPost, "/api/v1/action", nil)
	if origin != "" {
		wanted.Header.Set("Origin", origin)
	}
	handler.ServeHTTP(recorder, wanted)
	return recorder
}

func TestAuthenticatedMemberWithNoPermissionReachesHandler(t *testing.T) {
	t.Parallel()
	answer := request(router(t, principal(t, authz.Viewer), "", nil), "https://console.example.com")
	if answer.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", answer.Code)
	}
}

func TestMissingAuthenticationStopsBeforeHandler(t *testing.T) {
	t.Parallel()
	answer := request(router(t, authz.Principal{}, "", nil), "https://console.example.com")
	if answer.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", answer.Code)
	}
}

func TestCurrentRoleMustGrantDeclaredPermission(t *testing.T) {
	t.Parallel()
	var recorded []audit.Event
	answer := request(router(t, principal(t, authz.Viewer), authz.RelayConflictClear, &recorded),
		"https://console.example.com")
	if answer.Code != http.StatusForbidden || !strings.Contains(answer.Body.String(), string(authz.RelayConflictClear)) {
		t.Fatalf("answer = %d %q", answer.Code, answer.Body.String())
	}
	if len(recorded) != 1 || recorded[0].Organization != organizationID {
		t.Fatalf("recorded refusals = %+v", recorded)
	}

	allowed := request(router(t, principal(t, authz.Admin), authz.RelayConflictClear, nil),
		"https://console.example.com")
	if allowed.Code != http.StatusNoContent {
		t.Fatalf("admin status = %d, want 204", allowed.Code)
	}
}

func TestUnsafeCookieRequestRequiresAllowedOrigin(t *testing.T) {
	t.Parallel()
	for _, origin := range []string{"", "https://evil.example.com"} {
		if answer := request(router(t, principal(t, authz.Admin), "", nil), origin); answer.Code != http.StatusForbidden {
			t.Errorf("origin %q status = %d, want 403", origin, answer.Code)
		}
	}
}

func TestSafeCookieRequestDoesNotRequireOrigin(t *testing.T) {
	t.Parallel()
	handler, err := authz.Router([]authz.Route{{
		Method: http.MethodGet, Pattern: "/api/v1/action",
		Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusNoContent)
		}),
	}}, authz.Guard{
		Resolve: func(*http.Request) (authz.Principal, error) {
			return principal(t, authz.Admin), nil
		},
		Origin: "https://console.example.com",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	wanted := httptest.NewRequest(http.MethodGet, "/api/v1/action", nil)
	handler.ServeHTTP(recorder, wanted)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", recorder.Code)
	}
}

func TestRouterRejectsInvalidRoutes(t *testing.T) {
	t.Parallel()
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	valid := authz.Route{Method: http.MethodGet, Pattern: "/api/v1/session", Handler: handler}
	for _, testCase := range []struct {
		name   string
		routes []authz.Route
		want   string
	}{
		{"empty table", nil, "empty"},
		{"missing method", []authz.Route{{Pattern: "/api/v1/session", Handler: handler}}, "method"},
		{"relative pattern", []authz.Route{{Method: http.MethodGet, Pattern: "session", Handler: handler}}, "absolute"},
		{"missing handler", []authz.Route{{Method: http.MethodGet, Pattern: "/api/v1/session"}}, "no handler"},
		{"duplicate", []authz.Route{valid, valid}, "twice"},
		{"undeclared permission", []authz.Route{{Method: http.MethodGet, Pattern: "/api/v1/session", Permission: "invented", Handler: handler}}, "undeclared"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			_, err := authz.Router(testCase.routes, authz.Guard{
				Resolve: func(*http.Request) (authz.Principal, error) { return authz.Principal{}, authz.ErrNoCredential },
			})
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("invalid routes error = %v, want %q", err, testCase.want)
			}
		})
	}
}
