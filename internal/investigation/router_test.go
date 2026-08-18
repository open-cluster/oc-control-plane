package investigation

import (
	"context"
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// The router in isolation: deterministic, explainable, and honest about what it can
// read. The catalog is the stub type from runner_test.

func TestRouteSelectsByTermOverlapAndExplainsIt(t *testing.T) {
	t.Parallel()

	catalog := stubType(t, nil)
	payments := stubIntegration("Payments Slack")
	unrelated := stubIntegration("Random Room")
	labelled := stubIntegration("Ops")
	labelled.Labels = map[string]string{"service": "payments"}

	selected, remainder := route(catalog,
		[]integrations.Integration{unrelated, payments, labelled},
		subjectTerms("payments checkout latency"))

	if len(selected) != selectedSources {
		t.Fatalf("selected %d, want %d", len(selected), selectedSources)
	}
	names := selected[0].integration.Name + " " + selected[1].integration.Name
	if !strings.Contains(names, "Payments Slack") || !strings.Contains(names, "Ops") {
		t.Errorf("selected %q; both term matches outrank the unrelated room", names)
	}
	for _, choice := range selected {
		if !strings.Contains(choice.reason, "payments") {
			t.Errorf("reason %q does not name the term that chose it", choice.reason)
		}
	}
	if len(remainder) != 1 || remainder[0].integration.Name != "Random Room" {
		t.Errorf("remainder = %+v", remainder)
	}
}

func TestRouteIsStableAcrossRuns(t *testing.T) {
	t.Parallel()

	catalog := stubType(t, nil)
	candidates := []integrations.Integration{
		stubIntegration("Alpha"), stubIntegration("Beta"), stubIntegration("Gamma"),
	}
	terms := subjectTerms("nothing matches any name")

	first, _ := route(catalog, candidates, terms)
	second, _ := route(catalog, candidates, terms)
	if first[0].integration.Name != second[0].integration.Name ||
		first[1].integration.Name != second[1].integration.Name {
		t.Errorf("two identical routings chose differently: %q,%q then %q,%q",
			first[0].integration.Name, first[1].integration.Name,
			second[0].integration.Name, second[1].integration.Name)
	}
}

func TestRouteSkipsDisabledAndToollessCandidates(t *testing.T) {
	t.Parallel()

	catalog := stubType(t, nil)
	disabled := stubIntegration("Disabled Slack")
	disabled.DisabledAt = disabled.CreatedAt.Add(1)
	toolless := stubIntegration("Alertmanager")
	toolless.Type = 1

	selected, remainder := route(catalog,
		[]integrations.Integration{disabled, toolless}, nil)
	if len(selected) != 0 || len(remainder) != 0 {
		t.Errorf("selected %+v remainder %+v; neither can be read", selected, remainder)
	}
}

func TestSubjectTermsDropNoiseAndDuplicates(t *testing.T) {
	t.Parallel()

	terms := subjectTerms("What is wrong with the payments PAYMENTS pod?", "payments")
	joined := strings.Join(terms, " ")
	if strings.Count(joined, "payments") != 1 {
		t.Errorf("terms = %v; duplicates must collapse", terms)
	}
	for _, noise := range []string{"what", "the", "is"} {
		if strings.Contains(" "+joined+" ", " "+noise+" ") {
			t.Errorf("terms = %v carry the noise word %q", terms, noise)
		}
	}
	if !strings.Contains(joined, "pod") {
		t.Errorf("terms = %v; a three-letter identifier is signal", terms)
	}
}

// Tool availability derives from verified reality: a tool whose Requires are not all
// among the integration's recorded grants is absent from the selection's tool set —
// never a call that always fails. Nothing recorded offers only ungated tools.
func TestRouteOffersOnlyToolsTheVerifiedGrantsSupport(t *testing.T) {
	t.Parallel()

	catalog, err := integrations.NewCatalog(integrations.Definition{
		ID: 99, Key: "stub", Name: "Stub", Category: integrations.CategoryAlerting,
		Capabilities: []string{"stub.read", "stub.search"},
		Probe: func(context.Context, integrations.ProbeInput) integrations.Verification {
			return integrations.Verification{Status: integrations.StatusActive}
		},
		Tools: []integrations.Tool{
			{
				Name: "stub.read", Capability: "stub.read", Description: "reads",
				WhenToUse: "always", WhenNotToUse: "never", Permissions: "none",
				Output: "items",
				Run: func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
					return integrations.ToolResult{}, nil
				},
			},
			{
				Name: "stub.search", Capability: "stub.search", Description: "searches",
				WhenToUse: "sometimes", WhenNotToUse: "never twice", Permissions: "search",
				Output:   "matches",
				Requires: []string{"search:read", "user_token"},
				Run: func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
					return integrations.ToolResult{}, nil
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	granted := stubIntegration("Fully Granted")
	granted.VerifyGrants = []string{"search:read", "user_token", "channels:read"}
	partial := stubIntegration("Bot Token")
	partial.VerifyGrants = []string{"search:read"}
	unrecorded := stubIntegration("Never Verified")

	toolNames := func(candidate integrations.Integration) []string {
		selected, _ := route(catalog, []integrations.Integration{candidate}, nil)
		if len(selected) != 1 {
			t.Fatalf("selected %d candidates", len(selected))
		}
		var names []string
		for _, tool := range selected[0].tools {
			names = append(names, tool.Name)
		}
		return names
	}

	if names := toolNames(granted); len(names) != 2 {
		t.Errorf("full grants offer everything, got %v", names)
	}
	if names := toolNames(partial); len(names) != 1 || names[0] != "stub.read" {
		t.Errorf("partial grants must drop the gated tool, got %v", names)
	}
	if names := toolNames(unrecorded); len(names) != 1 || names[0] != "stub.read" {
		t.Errorf("nothing recorded offers only ungated tools, got %v", names)
	}
}

// A candidate whose grants support no tool at all is not a readable source.
func TestRouteSkipsACandidateWithNoOfferableTool(t *testing.T) {
	t.Parallel()

	catalog, err := integrations.NewCatalog(integrations.Definition{
		ID: 99, Key: "stub", Name: "Stub", Category: integrations.CategoryAlerting,
		Capabilities: []string{"stub.search"},
		Probe: func(context.Context, integrations.ProbeInput) integrations.Verification {
			return integrations.Verification{Status: integrations.StatusActive}
		},
		Tools: []integrations.Tool{{
			Name: "stub.search", Capability: "stub.search", Description: "searches",
			WhenToUse: "sometimes", WhenNotToUse: "never twice", Permissions: "search",
			Output:   "matches",
			Requires: []string{"user_token"},
			Run: func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
				return integrations.ToolResult{}, nil
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	selected, remainder := route(catalog,
		[]integrations.Integration{stubIntegration("Bot Only")}, nil)
	if len(selected) != 0 || len(remainder) != 0 {
		t.Errorf("a candidate with no offerable tool was selected: %+v %+v",
			selected, remainder)
	}
}
