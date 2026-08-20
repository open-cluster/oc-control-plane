package investigation

import (
	"context"
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// The offer's derivations: grant-filtered tool availability and the subject terms the
// handlers match questions against.

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
// among the integration's recorded grants is absent from the offered tool set — never a
// call that always fails. Nothing recorded offers only ungated tools, and a candidate
// whose grants support no tool at all is not a readable source.
func TestTheOfferHoldsOnlyToolsTheVerifiedGrantsSupport(t *testing.T) {
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
		offered := offeredSources(catalog, []integrations.Integration{candidate})
		if len(offered) != 1 {
			t.Fatalf("offered %d sources", len(offered))
		}
		var names []string
		for _, tool := range offered[0].Tools {
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

	searchOnly := integrations.Definition{
		ID: 98, Key: "gated", Name: "Gated", Category: integrations.CategoryAlerting,
		Capabilities: []string{"gated.search"},
		Probe: func(context.Context, integrations.ProbeInput) integrations.Verification {
			return integrations.Verification{Status: integrations.StatusActive}
		},
		Tools: []integrations.Tool{{
			Name: "gated.search", Capability: "gated.search", Description: "searches",
			WhenToUse: "sometimes", WhenNotToUse: "never twice", Permissions: "search",
			Output:   "matches",
			Requires: []string{"user_token"},
			Run: func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
				return integrations.ToolResult{}, nil
			},
		}},
	}
	gatedCatalog, err := integrations.NewCatalog(searchOnly)
	if err != nil {
		t.Fatal(err)
	}
	botOnly := integrations.Integration{Type: 98, Name: "Bot Only"}
	if offered := offeredSources(gatedCatalog,
		[]integrations.Integration{botOnly}); len(offered) != 0 {
		t.Errorf("a candidate with no offerable tool was offered: %+v", offered)
	}
}
