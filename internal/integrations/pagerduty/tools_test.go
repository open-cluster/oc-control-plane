package pagerduty

import (
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
		Credential: "key-under-test",
		Arguments:  args,
	})
}

func TestListIncidentsReturnsWhatTheVendorAnswers(t *testing.T) {
	t.Parallel()

	fake := newFakePagerDuty(t)
	fake.answer("/incidents", `{"incidents":[
		{"id":"PT4KHLK","incident_number":1234,"title":"The server is on fire.","status":"triggered","urgency":"high"}
	],"more":false}`)

	result, err := run(t, NewClient(fake.URL), "pagerduty.list_incidents",
		map[string]any{"status": "triggered", "urgency": "high"})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	selected, isTyped := result.Content.([]incidentContent)
	if !isTyped {
		t.Fatalf("content is %T", result.Content)
	}
	if len(selected) != 1 || selected[0].ID != "PT4KHLK" {
		t.Errorf("selection = %+v", selected)
	}
	if len(result.Sources) != 1 || result.Sources[0] != "PT4KHLK" {
		t.Errorf("sources = %v", result.Sources)
	}
}

func TestListIncidentsRefusesAnInvalidStatus(t *testing.T) {
	t.Parallel()

	_, err := run(t, NewClient(""), "pagerduty.list_incidents", map[string]any{"status": "exploded"})
	if err == nil {
		t.Fatal("err = nil, want a refusal: exploded is not one of the vendor's statuses")
	}
}

func TestListIncidentsRefusesAnUndeclaredArgument(t *testing.T) {
	t.Parallel()

	_, err := run(t, NewClient(""), "pagerduty.list_incidents", map[string]any{"acknowledge": true})
	if err == nil {
		t.Fatal("err = nil, want a refusal: acknowledge is not a declared argument")
	}
}

func TestGetIncidentReturnsOneWhole(t *testing.T) {
	t.Parallel()

	fake := newFakePagerDuty(t)
	fake.answer("/incidents/PT4KHLK",
		`{"incident":{"id":"PT4KHLK","incident_number":1234,"title":"The server is on fire.","status":"acknowledged"}}`)

	result, err := run(t, NewClient(fake.URL), "pagerduty.get_incident", map[string]any{"incidentId": "PT4KHLK"})
	if err != nil {
		t.Fatalf("getting: %v", err)
	}
	incident, isTyped := result.Content.(incidentContent)
	if !isTyped {
		t.Fatalf("content is %T", result.Content)
	}
	if incident.ID != "PT4KHLK" || incident.Status != "acknowledged" {
		t.Errorf("incident = %+v", incident)
	}
}

func TestGetIncidentRequiresAnID(t *testing.T) {
	t.Parallel()

	_, err := run(t, NewClient(""), "pagerduty.get_incident", nil)
	if err == nil {
		t.Fatal("err = nil, want a refusal: incidentId is required")
	}
}
