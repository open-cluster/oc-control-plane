package newrelic

import (
	"net/http"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

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
	t *testing.T, client *Client, name string, args map[string]any,
) (integrations.ToolResult, error) {
	t.Helper()
	return toolNamed(t, client, name).Run(testContext(t), integrations.ToolRequest{
		Integration: integrations.Integration{
			Configuration: map[string]any{"region": "us", "accountId": float64(123)},
		},
		Credential: "key-under-test",
		Arguments:  args,
	})
}

func TestListIssuesReturnsWhatTheVendorAnswers(t *testing.T) {
	t.Parallel()

	fake := newFakeNerdGraph(t)
	fake.answer = func(graphqlRequest) (int, string) {
		return http.StatusOK, `{"data":{"actor":{"account":{"aiIssues":{"issues":{"issues":[
			{"issueId":"1","title":"checkout down","priority":"CRITICAL","state":"ACTIVATED"}
		],"nextCursor":null}}}}}}`
	}

	result, err := run(t, NewClient(fake.URL), "newrelic.list_issues", map[string]any{"priority": "CRITICAL"})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	selected, isTyped := result.Content.([]issueContent)
	if !isTyped {
		t.Fatalf("content is %T", result.Content)
	}
	if len(selected) != 1 || selected[0].ID != "1" {
		t.Errorf("selection = %+v", selected)
	}
	if len(result.Sources) != 1 || result.Sources[0] != "1" {
		t.Errorf("sources = %v", result.Sources)
	}
}

func TestListIssuesRefusesAnInvalidPriority(t *testing.T) {
	t.Parallel()

	_, err := run(t, NewClient(""), "newrelic.list_issues", map[string]any{"priority": "URGENT"})
	if err == nil {
		t.Fatal("err = nil, want a refusal: URGENT is not one of the vendor's priorities")
	}
}

func TestListIssuesRefusesAnUndeclaredArgument(t *testing.T) {
	t.Parallel()

	_, err := run(t, NewClient(""), "newrelic.list_issues", map[string]any{"acknowledge": true})
	if err == nil {
		t.Fatal("err = nil, want a refusal: acknowledge is not a declared argument")
	}
}

func TestListIssuesRefusesAnIntegrationWithNoAccountId(t *testing.T) {
	t.Parallel()

	_, err := toolNamed(t, NewClient(""), "newrelic.list_issues").Run(testContext(t), integrations.ToolRequest{
		Integration: integrations.Integration{Configuration: map[string]any{"region": "us"}},
		Credential:  "key",
	})
	if err == nil {
		t.Fatal("err = nil, want a refusal: no accountId is configured")
	}
}

func TestGetIssueReturnsOneWhole(t *testing.T) {
	t.Parallel()

	fake := newFakeNerdGraph(t)
	fake.answer = func(graphqlRequest) (int, string) {
		return http.StatusOK, `{"data":{"actor":{"account":{"aiIssues":{"issues":{"issues":[
			{"issueId":"9","title":"panic","priority":"HIGH","state":"ACTIVATED"}
		]}}}}}}`
	}

	result, err := run(t, NewClient(fake.URL), "newrelic.get_issue", map[string]any{"issueId": "9"})
	if err != nil {
		t.Fatalf("getting: %v", err)
	}
	issue, isTyped := result.Content.(issueContent)
	if !isTyped {
		t.Fatalf("content is %T", result.Content)
	}
	if issue.ID != "9" || issue.Priority != "HIGH" {
		t.Errorf("issue = %+v", issue)
	}
}

func TestGetIssueRequiresAnID(t *testing.T) {
	t.Parallel()

	_, err := run(t, NewClient(""), "newrelic.get_issue", nil)
	if err == nil {
		t.Fatal("err = nil, want a refusal: issueId is required")
	}
}
