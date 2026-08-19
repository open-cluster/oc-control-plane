package datadog

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
	t *testing.T, client *Client, name string, site string, args map[string]any,
) (integrations.ToolResult, error) {
	t.Helper()
	return toolNamed(t, client, name).Run(testContext(t), integrations.ToolRequest{
		Integration: integrations.Integration{Configuration: map[string]any{"site": site}},
		Credential:  testCredentialJSON(t),
		Arguments:   args,
	})
}

func TestListMonitorsReturnsWhatTheVendorAnswers(t *testing.T) {
	t.Parallel()

	fake := newFakeDatadog(t)
	fake.answer("/api/v1/monitor", `[
		{"id":1,"name":"checkout latency","overall_state":"Alert"},
		{"id":2,"name":"checkout errors","overall_state":"OK"}
	]`)

	result, err := run(t, NewClient(fake.URL), "datadog.list_monitors", "datadoghq.com",
		map[string]any{"tags": "service:checkout"})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	selected, isTyped := result.Content.([]monitorContent)
	if !isTyped {
		t.Fatalf("content is %T", result.Content)
	}
	if len(selected) != 2 || selected[0].Name != "checkout latency" {
		t.Errorf("selection = %+v", selected)
	}
	if len(result.Sources) != 2 || result.Sources[0] != "1" {
		t.Errorf("sources = %v; provenance must cite the monitor ids read", result.Sources)
	}
}

func TestListMonitorsAppliesTheDefaultBound(t *testing.T) {
	t.Parallel()

	fake := newFakeDatadog(t)
	fake.answers["/api/v1/monitor"] = func(writer http.ResponseWriter, request *http.Request) {
		if got := request.URL.Query().Get("page_size"); got != "25" {
			t.Errorf("an unstated limit reached the vendor as %q, want the named default", got)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`[]`))
	}

	if _, err := run(t, NewClient(fake.URL), "datadog.list_monitors", "datadoghq.com", nil); err != nil {
		t.Fatalf("listing: %v", err)
	}
}

func TestListMonitorsRefusesAnUndeclaredArgument(t *testing.T) {
	t.Parallel()

	_, err := run(t, NewClient(""), "datadog.list_monitors", "datadoghq.com",
		map[string]any{"resolve": true})
	if err == nil {
		t.Fatal("err = nil, want a refusal: resolve is not a declared argument")
	}
}

func TestListMonitorsRefusesAnIntegrationWithNoSite(t *testing.T) {
	t.Parallel()

	_, err := toolNamed(t, NewClient(""), "datadog.list_monitors").Run(testContext(t), integrations.ToolRequest{
		Integration: integrations.Integration{Configuration: map[string]any{}},
		Credential:  testCredentialJSON(t),
	})
	if err == nil {
		t.Fatal("err = nil, want a refusal: no site is configured")
	}
}

func TestGetMonitorReturnsOneWhole(t *testing.T) {
	t.Parallel()

	fake := newFakeDatadog(t)
	fake.answer("/api/v1/monitor/42", `{"id":42,"name":"checkout latency","overall_state":"Alert"}`)

	result, err := run(t, NewClient(fake.URL), "datadog.get_monitor", "datadoghq.com",
		map[string]any{"monitorId": float64(42)})
	if err != nil {
		t.Fatalf("getting: %v", err)
	}
	monitor, isTyped := result.Content.(monitorContent)
	if !isTyped {
		t.Fatalf("content is %T", result.Content)
	}
	if monitor.ID != 42 || monitor.OverallState != "Alert" {
		t.Errorf("monitor = %+v", monitor)
	}
}

func TestGetMonitorRequiresAnID(t *testing.T) {
	t.Parallel()

	_, err := run(t, NewClient(""), "datadog.get_monitor", "datadoghq.com", nil)
	if err == nil {
		t.Fatal("err = nil, want a refusal: monitorId is required")
	}
}
