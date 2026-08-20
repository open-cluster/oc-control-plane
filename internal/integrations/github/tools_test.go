package github

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

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
		// Full vendor pages: the filter walk selects client-side, so the page size is
		// the vendor's ceiling, not the caller's bound.
		if got := request.URL.Query().Get("per_page"); got != "100" {
			t.Errorf("per_page = %q, want a full page", got)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"total_count":3,"repositories":[
			{"id":1296269,"name":"payments","full_name":"acme-corp/payments",
			 "private":true,"default_branch":"main","description":"the payments service"},
			{"id":1296270,"name":"deploy","full_name":"acme-corp/deploy"},
			{"id":1296271,"name":"website","full_name":"acme-corp/website"}]}`))
	}

	result, err := run(t, appAgainst(t, fake), NewClient(fake.URL),
		"github.list_repositories", map[string]any{"limit": float64(2)})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	listed, isTyped := result.Content.([]repositoryContent)
	if !isTyped || len(listed) != 2 || listed[0].ID != 1296269 {
		t.Fatalf("content = %+v", result.Content)
	}
	if !result.Truncated {
		t.Error("3 matched behind a bound of 2 must read as truncated")
	}

	t.Run("a whole walk answers untruncated", func(t *testing.T) {
		result, err := run(t, appAgainst(t, fake), NewClient(fake.URL),
			"github.list_repositories", nil)
		if err != nil {
			t.Fatalf("listing: %v", err)
		}
		if result.Truncated {
			t.Error("the walk saw the installation's every page; claiming truncation " +
				"over-claims")
		}
	})
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

func TestListRepositoriesFiltersInsideAPaginationWalk(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	healthyInstallation(fake)
	fake.answers["/installation/repositories"] = func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Query().Get("page") == "2" {
			_, _ = writer.Write([]byte(`{"total_count":2,"repositories":[
				{"id":2,"name":"payments","full_name":"acme-corp/payments"}]}`))
			return
		}
		writer.Header().Set("Link", `<next>; rel="next"`)
		_, _ = writer.Write([]byte(`{"total_count":2,"repositories":[
			{"id":1,"name":"website","full_name":"acme-corp/website"}]}`))
	}

	result, err := run(t, appAgainst(t, fake), NewClient(fake.URL),
		"github.list_repositories", map[string]any{"nameContains": "payments"})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	listed := result.Content.([]repositoryContent)
	if len(listed) != 1 || listed[0].ID != 2 {
		t.Errorf("content = %+v; a match beyond page one must be found", result.Content)
	}
}

func TestReadPullRequestCarriesIntentFilesAndChecks(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	healthyInstallation(fake)
	grantsPayments(fake)
	fake.answer("/repos/acme-corp/payments/pulls/98", `
		{"number":98,"title":"Retry writes on conflict","state":"closed",
		 "body":"Conflict storms were exhausting the pool.","merged":true,
		 "merged_at":"2026-08-15T21:00:00Z","updated_at":"2026-08-15T21:00:00Z",
		 "user":{"login":"kai-dev"},"head":{"ref":"fix/retry","sha":"abc123"},
		 "base":{"ref":"main"},"html_url":"https://github.com/acme-corp/payments/pull/98"}`)
	fake.answer("/repos/acme-corp/payments/pulls/98/files", `[
		{"filename":"config/pool.yaml","status":"modified","additions":1,"deletions":1,
		 "patch":"@@ -1 +1 @@\n-max: 10\n+max: 100"}]`)
	fake.answer("/repos/acme-corp/payments/commits/abc123/check-runs", `
		{"total_count":1,"check_runs":[
			{"name":"unit tests","status":"completed","conclusion":"failure"}]}`)

	result, err := run(t, appAgainst(t, fake), NewClient(fake.URL),
		"github.read_pull_request",
		map[string]any{"repositoryId": float64(1296269), "number": float64(98)})
	if err != nil {
		t.Fatalf("reading the pull request: %v", err)
	}
	content := result.Content.(map[string]any)
	if content["description"] != "Conflict storms were exhausting the pool." {
		t.Errorf("description = %v; the intent is the point of this tool", content["description"])
	}
	files := content["files"].([]changedFileContent)
	if len(files) != 1 || !strings.Contains(files[0].Patch, "max: 100") {
		t.Errorf("files = %+v", files)
	}
	checks := content["checks"].([]map[string]string)
	if len(checks) != 1 || checks[0]["conclusion"] != "failure" {
		t.Errorf("checks = %+v; CI's objection is what this read is for", checks)
	}
}

func TestReadPullRequestNamesUnreadableChecksInsteadOfFailing(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	healthyInstallation(fake)
	grantsPayments(fake)
	fake.answer("/repos/acme-corp/payments/pulls/98", `
		{"number":98,"title":"t","state":"open","body":"b","updated_at":"2026-08-15T21:00:00Z",
		 "head":{"ref":"h","sha":"abc123"},"base":{"ref":"main"}}`)
	fake.answer("/repos/acme-corp/payments/pulls/98/files", `[]`)
	fake.answers["/repos/acme-corp/payments/commits/abc123/check-runs"] = func(
		writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusForbidden)
		_, _ = writer.Write([]byte(`{"message":"Resource not accessible by integration"}`))
	}

	result, err := run(t, appAgainst(t, fake), NewClient(fake.URL),
		"github.read_pull_request",
		map[string]any{"repositoryId": float64(1296269), "number": float64(98)})
	if err != nil {
		t.Fatalf("unreadable checks must not fail the whole read: %v", err)
	}
	content := result.Content.(map[string]any)
	if note, _ := content["checksUnavailable"].(string); !strings.Contains(note, "could not be read") {
		t.Errorf("content = %v; the absence must be named", content)
	}
}

func TestReadCommitBoundsThePatches(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	healthyInstallation(fake)
	grantsPayments(fake)
	long := strings.Repeat("+padding line\n", 400)
	fake.answer("/repos/acme-corp/payments/commits/abc123", `
		{"sha":"abc123","commit":{"message":"big change",
		 "author":{"name":"Kai","date":"2026-08-15T20:00:00Z"}},
		 "html_url":"https://github.com/acme-corp/payments/commit/abc123",
		 "files":[{"filename":"a.go","status":"modified","additions":400,"deletions":0,
		           "patch":"`+strings.ReplaceAll(long, "\n", `\n`)+`"}]}`)

	result, err := run(t, appAgainst(t, fake), NewClient(fake.URL), "github.read_commit",
		map[string]any{"repositoryId": float64(1296269), "sha": "abc123"})
	if err != nil {
		t.Fatalf("reading the commit: %v", err)
	}
	files := result.Content.(map[string]any)["files"].([]changedFileContent)
	if len(files) != 1 || !files[0].PatchTruncated ||
		!strings.Contains(files[0].Patch, "patch cut at") {
		t.Errorf("files = %+v; an oversized patch is cut with the cut named", files)
	}
	if len(files[0].Patch) > maxPatchBytes+64 {
		t.Errorf("patch is %d bytes, past the bound", len(files[0].Patch))
	}
}

func TestReadWorkflowRunsClampsTheWindow(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	healthyInstallation(fake)
	grantsPayments(fake)
	fake.answers["/repos/acme-corp/payments/actions/runs"] = func(writer http.ResponseWriter, request *http.Request) {
		created := request.URL.Query().Get("created")
		if !strings.HasPrefix(created, "2026-08-15T20:00:00Z..") {
			t.Errorf("created = %q; a wider ask must clamp to the investigation's window", created)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"total_count":0,"workflow_runs":[]}`))
	}

	tool := toolNamed(t, appAgainst(t, fake), NewClient(fake.URL), "github.read_workflow_runs")
	_, err := tool.Run(testContext(t), integrations.ToolRequest{
		Integration: integrations.Integration{
			Configuration: map[string]any{"installationId": float64(77)},
		},
		Arguments: map[string]any{
			"repositoryId": float64(1296269),
			"since":        "2026-08-15T10:00:00Z",
		},
		WindowFrom:  mustTime(t, "2026-08-15T20:00:00Z"),
		WindowUntil: mustTime(t, "2026-08-15T22:00:00Z"),
	})
	if err != nil {
		t.Fatalf("reading runs: %v", err)
	}
}

func TestReadJobLogReadsTheFailingJobsTail(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	healthyInstallation(fake)
	grantsPayments(fake)
	fake.answer("/repos/acme-corp/payments/actions/runs/42/jobs", `
		{"jobs":[{"id":8,"name":"lint","status":"completed","conclusion":"success","steps":[]},
		         {"id":9,"name":"test","status":"completed","conclusion":"failure",
		          "steps":[{"name":"go test","conclusion":"failure"}]}]}`)
	fake.answers["/repos/acme-corp/payments/actions/jobs/9/logs"] = func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, fake.URL+"/logstore", http.StatusFound)
	}
	fake.answers["/logstore"] = func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("--- FAIL: TestPool (0.02s)\n"))
	}

	result, err := run(t, appAgainst(t, fake), NewClient(fake.URL), "github.read_job_log",
		map[string]any{"repositoryId": float64(1296269), "runId": float64(42)})
	if err != nil {
		t.Fatalf("reading the log: %v", err)
	}
	content := result.Content.(map[string]any)
	if content["job"] != "test" || content["failedStep"] != "go test" {
		t.Errorf("content = %v; the failing job chooses itself", content)
	}
	if !strings.Contains(content["logTail"].(string), "FAIL: TestPool") {
		t.Errorf("logTail = %v", content["logTail"])
	}
}

func TestReadFileIsBoundedAndAddressedByRef(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	healthyInstallation(fake)
	grantsPayments(fake)
	fake.answers["/repos/acme-corp/payments/contents/config/pool.yaml"] = func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("max: 100\n"))
	}

	result, err := run(t, appAgainst(t, fake), NewClient(fake.URL), "github.read_file",
		map[string]any{
			"repositoryId": float64(1296269), "path": "config/pool.yaml", "ref": "abc123",
		})
	if err != nil {
		t.Fatalf("reading the file: %v", err)
	}
	content := result.Content.(map[string]any)
	if content["content"] != "max: 100\n" || content["ref"] != "abc123" {
		t.Errorf("content = %v", content)
	}
}

func TestListReleasesSaysWhatShipped(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	healthyInstallation(fake)
	grantsPayments(fake)
	fake.answer("/repos/acme-corp/payments/releases", `[
		{"name":"v2.3.0","tag_name":"v2.3.0","published_at":"2026-08-15T19:00:00Z",
		 "prerelease":false,"author":{"login":"kai-dev"},
		 "html_url":"https://github.com/acme-corp/payments/releases/tag/v2.3.0"}]`)

	result, err := run(t, appAgainst(t, fake), NewClient(fake.URL), "github.list_releases",
		map[string]any{"repositoryId": float64(1296269)})
	if err != nil {
		t.Fatalf("listing releases: %v", err)
	}
	releases := result.Content.([]map[string]any)
	if len(releases) != 1 || releases[0]["tag"] != "v2.3.0" {
		t.Errorf("releases = %+v", releases)
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
