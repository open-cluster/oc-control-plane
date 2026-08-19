package sentry

import (
	"net/http"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// The tools against the fake vendor: bounds applied, truncation surfaced, refusals in
// plain language. The Run functions under test are the real ones the catalog serves.

func toolNamed(t *testing.T, client *Client, name string) integrations.Tool {
	t.Helper()
	for _, tool := range tools(client) {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("no tool named %s", name)
	return integrations.Tool{}
}

func run(
	t *testing.T, client *Client, name string, organizationSlug string, args map[string]any,
) (integrations.ToolResult, error) {
	t.Helper()
	return toolNamed(t, client, name).Run(testContext(t), integrations.ToolRequest{
		Integration: integrations.Integration{
			Configuration: map[string]any{"organizationSlug": organizationSlug},
		},
		Credential: "token-under-test",
		Arguments:  args,
	})
}

func TestListIssuesReturnsWhatTheVendorAnswers(t *testing.T) {
	t.Parallel()

	fake := newFakeSentry(t)
	fake.answer("/organizations/acme/issues/", `[
		{"id":"1","shortId":"ACME-1","title":"nil pointer","level":"error","count":"9"},
		{"id":"2","shortId":"ACME-2","title":"timeout","level":"warning","count":"2"}
	]`)

	result, err := run(t, NewClient(fake.URL), "sentry.list_issues", "acme",
		map[string]any{"query": "is:unresolved"})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	selected, isTyped := result.Content.([]issueContent)
	if !isTyped {
		t.Fatalf("content is %T", result.Content)
	}
	if len(selected) != 2 || selected[0].ShortID != "ACME-1" {
		t.Errorf("selection = %+v", selected)
	}
	if len(result.Sources) != 2 || result.Sources[0] != "1" {
		t.Errorf("sources = %v; provenance must cite the issue ids read", result.Sources)
	}
}

func TestListIssuesAppliesTheDefaultBound(t *testing.T) {
	t.Parallel()

	fake := newFakeSentry(t)
	fake.answers["/organizations/acme/issues/"] = func(writer http.ResponseWriter, request *http.Request) {
		if got := request.URL.Query().Get("limit"); got != "25" {
			t.Errorf("an unstated limit reached the vendor as %q, want the named default", got)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`[]`))
	}

	if _, err := run(t, NewClient(fake.URL), "sentry.list_issues", "acme", nil); err != nil {
		t.Fatalf("listing: %v", err)
	}
}

func TestListIssuesRefusesAnUndeclaredArgument(t *testing.T) {
	t.Parallel()

	_, err := run(t, NewClient(""), "sentry.list_issues", "acme", map[string]any{"resolve": true})
	if err == nil {
		t.Fatal("err = nil, want a refusal: resolve is not a declared argument")
	}
}

func TestListIssuesRefusesAnIntegrationWithNoOrganizationSlug(t *testing.T) {
	t.Parallel()

	_, err := toolNamed(t, NewClient(""), "sentry.list_issues").Run(testContext(t), integrations.ToolRequest{
		Integration: integrations.Integration{Configuration: map[string]any{}},
		Credential:  "token",
	})
	if err == nil {
		t.Fatal("err = nil, want a refusal: no organizationSlug is configured")
	}
}

func TestGetIssueReturnsOneWhole(t *testing.T) {
	t.Parallel()

	fake := newFakeSentry(t)
	fake.answer("/organizations/acme/issues/123/",
		`{"id":"123","shortId":"ACME-9","title":"panic","culprit":"handler.go","status":"unresolved"}`)

	result, err := run(t, NewClient(fake.URL), "sentry.get_issue", "acme",
		map[string]any{"issueId": "123"})
	if err != nil {
		t.Fatalf("getting: %v", err)
	}
	issue, isTyped := result.Content.(issueContent)
	if !isTyped {
		t.Fatalf("content is %T", result.Content)
	}
	if issue.ShortID != "ACME-9" || issue.Culprit != "handler.go" {
		t.Errorf("issue = %+v", issue)
	}
	if len(result.Sources) != 1 || result.Sources[0] != "123" {
		t.Errorf("sources = %v", result.Sources)
	}
}

func TestGetIssueRequiresAnID(t *testing.T) {
	t.Parallel()

	_, err := run(t, NewClient(""), "sentry.get_issue", "acme", nil)
	if err == nil {
		t.Fatal("err = nil, want a refusal: issueId is required")
	}
}
