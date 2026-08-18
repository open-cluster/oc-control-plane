package github

import (
	"net/http"
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// The tools against the fake vendor: bounds applied, ids required, truncation surfaced,
// refusals in plain language. The Run functions under test are the real ones the catalog
// serves.

func toolNamed(t *testing.T, app *App, client *Client, name string) integrations.Tool {
	t.Helper()
	for _, tool := range tools(app, client) {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("no tool named %s", name)
	return integrations.Tool{}
}

func run(
	t *testing.T, app *App, client *Client, name string, args map[string]any,
) (integrations.ToolResult, error) {
	t.Helper()
	return toolNamed(t, app, client, name).Run(testContext(t), integrations.ToolRequest{
		Integration: integrations.Integration{
			Configuration: map[string]any{"installationId": float64(77)},
		},
		Arguments: args,
	})
}

func TestListRepositoriesReportsStableIDsAndTruncation(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	healthyInstallation(fake)
	fake.answers["/installation/repositories"] = func(writer http.ResponseWriter, request *http.Request) {
		if got := request.URL.Query().Get("per_page"); got != "50" {
			t.Errorf("per_page = %q, want the named default", got)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"total_count":120,"repositories":[
			{"id":1296269,"name":"payments","full_name":"acme-corp/payments",
			 "private":true,"default_branch":"main","description":"the payments service"}]}`))
	}

	result, err := run(t, appAgainst(t, fake), NewClient(fake.URL),
		"github.list_repositories", nil)
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	listed, isTyped := result.Content.([]repositoryContent)
	if !isTyped || len(listed) != 1 || listed[0].ID != 1296269 {
		t.Fatalf("content = %+v", result.Content)
	}
	if !result.Truncated {
		t.Error("120 selected behind a bound of 50 must read as truncated")
	}
}

func TestReadCommitsRequiresTheStableID(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	healthyInstallation(fake)

	_, err := run(t, appAgainst(t, fake), NewClient(fake.URL), "github.read_commits", nil)
	if err == nil || !strings.Contains(err.Error(), "repositoryId is required") {
		t.Fatalf("want a refusal naming the missing id, got %v", err)
	}

	_, err = run(t, appAgainst(t, fake), NewClient(fake.URL), "github.read_commits",
		map[string]any{"repositoryId": "acme-corp/payments"})
	if err == nil || !strings.Contains(err.Error(), "whole positive number") {
		t.Fatalf("a name where an id belongs must be refused — names break on rename: %v", err)
	}
}

func TestReadCommitsIsBoundedToTheWindow(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	healthyInstallation(fake)
	grantsPayments(fake)
	fake.answers["/repos/acme-corp/payments/commits"] = func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		if query.Get("since") != "2026-08-15T00:00:00Z" {
			t.Errorf("since = %q; the incident's own window must bound the read", query.Get("since"))
		}
		if query.Get("per_page") != "5" {
			t.Errorf("per_page = %q, want the caller's bound", query.Get("per_page"))
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`[{"sha":"aaa111",
			"commit":{"message":"raise the pool size",
			          "author":{"name":"Kai","date":"2026-08-15T20:00:00Z"}},
			"author":{"login":"kai-dev"}}]`))
	}

	result, err := run(t, appAgainst(t, fake), NewClient(fake.URL), "github.read_commits",
		map[string]any{
			"repositoryId": float64(1296269),
			"since":        "2026-08-15T00:00:00Z",
			"limit":        float64(5),
		})
	if err != nil {
		t.Fatalf("reading commits: %v", err)
	}
	read, isTyped := result.Content.([]commitContent)
	if !isTyped || len(read) != 1 || read[0].Author != "kai-dev" {
		t.Errorf("content = %+v", result.Content)
	}
}

func TestReadCommitsRefusesAnUnreadableWindow(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	healthyInstallation(fake)

	_, err := run(t, appAgainst(t, fake), NewClient(fake.URL), "github.read_commits",
		map[string]any{"repositoryId": float64(1296269), "since": "last tuesday"})
	if err == nil || !strings.Contains(err.Error(), "RFC 3339") {
		t.Fatalf("want a refusal naming the expected form, got %v", err)
	}
}

func TestReadPullRequestsSurfacesBranchesAndMergeTimes(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	healthyInstallation(fake)
	grantsPayments(fake)
	fake.answer("/repos/acme-corp/payments/pulls", `[
		{"number":98,"title":"Retry writes on conflict","state":"closed",
		 "merged_at":"2026-08-15T21:00:00Z","updated_at":"2026-08-15T21:00:00Z",
		 "user":{"login":"kai-dev"},"head":{"ref":"fix/retry"},"base":{"ref":"main"}}]`)

	result, err := run(t, appAgainst(t, fake), NewClient(fake.URL),
		"github.read_pull_requests", map[string]any{"repositoryId": float64(1296269)})
	if err != nil {
		t.Fatalf("reading pull requests: %v", err)
	}
	read, isTyped := result.Content.([]pullRequestContent)
	if !isTyped || len(read) != 1 || read[0].MergedAt == "" || read[0].Head != "fix/retry" {
		t.Errorf("content = %+v", result.Content)
	}
}

func TestAnUndeclaredArgumentIsRefusedByName(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	healthyInstallation(fake)

	_, err := run(t, appAgainst(t, fake), NewClient(fake.URL), "github.read_commits",
		map[string]any{"repositoryId": float64(1), "brach": "main"})
	if err == nil || !strings.Contains(err.Error(), "brach") {
		t.Fatalf("an undeclared argument must be refused by name, got %v", err)
	}
}

func TestToolsWithoutAConfiguredAppRefuseByName(t *testing.T) {
	t.Parallel()

	_, err := run(t, nil, NewClient("http://127.0.0.1:1"), "github.list_repositories", nil)
	if err == nil || !strings.Contains(err.Error(), "GitHub App") {
		t.Fatalf("an unconfigured deployment must refuse by name, got %v", err)
	}
}
