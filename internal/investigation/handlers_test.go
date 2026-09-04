package investigation

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
)

type directHTTPStore struct {
	HTTPStore
	found Investigation
	runs  []ToolRun
}

func (s directHTTPStore) Investigation(
	context.Context, tenancy.Organization, uuid.UUID,
) (Investigation, error) {
	return s.found, nil
}

func (s directHTTPStore) InvestigationToolRuns(
	context.Context, tenancy.Organization, uuid.UUID,
) ([]ToolRun, error) {
	return s.runs, nil
}

type unusedAgent struct{}

func (unusedAgent) Run(context.Context, tenancy.Organization, Investigation) error { return nil }

func TestDirectInvestigationCreationRejectsQuestions(t *testing.T) {
	t.Parallel()

	organization, err := tenancy.NewOrganization("acme")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := authz.NewPrincipal(authz.KindUser, "operator", "Operator", []authz.Membership{{
		Organization: organization,
		Role:         authz.Editor,
	}})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	router, err := authz.Router(Handlers{
		Store:  directHTTPStore{},
		Runner: &Runner{Agent: unusedAgent{}},
		Logger: logger,
	}.Routes(), authz.Guard{
		Resolve: func(*http.Request) (authz.Principal, error) { return principal, nil },
		ResolveOrganization: func(context.Context, tenancy.Organization) (bool, error) {
			return true, nil
		},
		Origins: []string{"https://console.example.com"},
		Logger:  logger,
	})
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/v1/investigations",
		bytes.NewBufferString(`{"question":"why is checkout slow?"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://console.example.com")
	request.Header.Set(authz.OrganizationHeader, organization.String())
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest ||
		!strings.Contains(recorder.Body.String(), "request body is not what this operation accepts") {
		t.Fatalf("opening from a question = %d, want strict-body 400: %s",
			recorder.Code, recorder.Body.String())
	}
}

func TestCanonicalInvestigationDetailContainsOnlyTheInvestigationAndToolRuns(t *testing.T) {
	t.Parallel()

	organization, err := tenancy.NewOrganization("acme")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := authz.NewPrincipal(authz.KindUser, "operator", "Operator", []authz.Membership{{
		Organization: organization,
		Role:         authz.Viewer,
	}})
	if err != nil {
		t.Fatal(err)
	}
	id, integrationID := uuid.New(), uuid.New()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	router, err := authz.Router(Handlers{
		Store: directHTTPStore{
			found: Investigation{
				ID: id, Status: StatusRunning,
				Spend: Spend{InputTokens: 10, MicroCents: 25},
			},
			runs: []ToolRun{{IntegrationID: integrationID, Ordinal: 1, Tool: "read"}},
		},
		Logger: logger,
	}.Routes(), authz.Guard{
		Resolve: func(*http.Request) (authz.Principal, error) { return principal, nil },
		ResolveOrganization: func(context.Context, tenancy.Organization) (bool, error) {
			return true, nil
		},
		Origins: []string{"https://console.example.com"},
		Logger:  logger,
	})
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/investigations/"+id.String(), nil)
	request.Header.Set(authz.OrganizationHeader, organization.String())
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("reading detail = %d: %s", recorder.Code, recorder.Body.String())
	}
	var detail map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if _, ok := detail["runs"]; !ok {
		t.Error("canonical detail has no durable Tool Runs")
	}
	usage, ok := detail["usage"].(map[string]any)
	if !ok || usage["inputTokens"] != float64(10) || usage["outputTokens"] != float64(0) ||
		len(usage) != 2 {
		t.Errorf("canonical detail token usage = %#v", detail["usage"])
	}
	for _, retired := range []string{"sources", "integrationId", "spend"} {
		if _, ok := detail[retired]; ok {
			t.Errorf("canonical detail still contains retired field %q", retired)
		}
	}
}
