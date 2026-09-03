package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

func stubIntegration(name string) integrations.Integration {
	return integrations.Integration{ID: uuid.New(), Type: 99, Name: name}
}

func TestTheOfferRequiresCurrentVerificationAndKeepsSameTypeSourcesReachable(t *testing.T) {
	t.Parallel()

	definition := integrations.Definition{
		Manifest: integrations.Manifest{ID: 99, Key: "stub", Name: "Stub",
			Category: integrations.CategoryAlerting, Available: true,
			Tools: []integrations.Tool{{
				Name: "stub.read", Description: "reads", WhenToUse: "when asked",
				WhenNotToUse: "without a question", Permissions: "read", Output: "items",
				Run: func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
					return integrations.ToolResult{}, nil
				},
			}}},
		Probe: func(context.Context, integrations.ProbeInput) integrations.Verification {
			return integrations.Verification{Status: integrations.StatusActive}
		},
	}
	catalog, err := integrations.NewCatalog(definition)
	if err != nil {
		t.Fatal(err)
	}
	first := stubIntegration("First")
	first.Status, first.LastVerifiedAt = integrations.StatusActive, time.Now().UTC()
	second := stubIntegration("Second")
	second.Status, second.LastVerifiedAt = integrations.StatusDegraded, time.Now().UTC()
	configured := stubIntegration("Configured")
	configured.Status, configured.LastVerifiedAt = integrations.StatusConfigured, time.Now().UTC()
	stale := stubIntegration("Stale")
	stale.Status, stale.LastVerifiedAt = integrations.StatusActive, time.Now().Add(-25*time.Hour)

	offered := offeredSources(catalog, []integrations.Integration{first, second, configured, stale})
	if len(offered) != 2 {
		t.Fatalf("offered %d sources, want only the two currently verified sources", len(offered))
	}
	selected := make([]selection, 0, len(offered))
	for _, source := range offered {
		selected = append(selected, selection{integration: source.Integration, tools: source.Tools})
	}
	seen := map[string]bool{}
	for _, source := range selected {
		if len(source.tools) != 1 || !strings.Contains(source.tools[0].Name, "__") {
			t.Fatalf("same-type Tool was not Integration-bound: %+v", source.tools)
		}
		resolved, _, ok := toolNamed(selected, source.tools[0].Name)
		if !ok || resolved.integration.ID != source.integration.ID {
			t.Fatalf("Tool %q did not resolve to Integration %s", source.tools[0].Name,
				source.integration.ID)
		}
		seen[source.integration.ID.String()] = true
	}
	if !seen[first.ID.String()] || !seen[second.ID.String()] {
		t.Fatalf("same-type Integrations were not both reachable: %v", seen)
	}
	if _, _, ok := toolNamed(selected, "stub.read"); ok {
		t.Fatal("an ambiguous bare Tool name must not resolve to either Integration")
	}
}

// Tool availability derives from verified reality: a tool whose Requires are not all
// among the integration's recorded grants is absent from the offered tool set — never a
// call that always fails. Nothing recorded offers only ungated tools, and a candidate
// whose grants support no tool at all is not a readable source.
func TestTheOfferHoldsOnlyToolsTheVerifiedGrantsSupport(t *testing.T) {
	t.Parallel()

	catalog, err := integrations.NewCatalog(integrations.Definition{
		Manifest: integrations.Manifest{ID: 99, Key: "stub", Name: "Stub",
			Category: integrations.CategoryAlerting, Available: true,
			Tools: []integrations.Tool{
				{
					Name: "stub.read", Description: "reads",
					WhenToUse: "always", WhenNotToUse: "never", Permissions: "none",
					Output: "items",
					Run: func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
						return integrations.ToolResult{}, nil
					},
				},
				{
					Name: "stub.search", Description: "searches",
					WhenToUse: "sometimes", WhenNotToUse: "never twice", Permissions: "search",
					Output: "matches", Requires: []string{"search:read", "user_token"},
					Run: func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
						return integrations.ToolResult{}, nil
					},
				},
			}},
		Probe: func(context.Context, integrations.ProbeInput) integrations.Verification {
			return integrations.Verification{Status: integrations.StatusActive}
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
		Manifest: integrations.Manifest{ID: 98, Key: "gated", Name: "Gated",
			Category: integrations.CategoryAlerting, Available: true,
			Tools: []integrations.Tool{{
				Name: "gated.search", Description: "searches",
				WhenToUse: "sometimes", WhenNotToUse: "never twice", Permissions: "search",
				Output: "matches", Requires: []string{"user_token"},
				Run: func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
					return integrations.ToolResult{}, nil
				},
			}}},
		Probe: func(context.Context, integrations.ProbeInput) integrations.Verification {
			return integrations.Verification{Status: integrations.StatusActive}
		},
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

func TestAConversationOriginOffersOnlyItsOwnThreadRead(t *testing.T) {
	t.Parallel()

	read := func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{}, nil
	}
	definition := integrations.Definition{
		Manifest: integrations.Manifest{ID: 99, Key: "chat", Name: "Chat",
			Category: integrations.CategoryAlerting, Available: true,
			Tools: []integrations.Tool{
				{
					Name: "chat.thread", Description: "reads the originating thread",
					WhenToUse: "for its thread", WhenNotToUse: "for another thread",
					Permissions: "history", Output: "messages", ConversationScoped: true, Run: read,
				},
				{
					Name: "chat.channel", Description: "reads an entire channel",
					WhenToUse: "when explicitly granted", WhenNotToUse: "for an implicit mention",
					Permissions: "history", Output: "messages", Run: read,
				},
			}},
		Probe: func(context.Context, integrations.ProbeInput) integrations.Verification {
			return integrations.Verification{Status: integrations.StatusActive}
		},
	}
	catalog, err := integrations.NewCatalog(definition)
	if err != nil {
		t.Fatal(err)
	}
	origin := stubIntegration("Origin workspace")
	other := stubIntegration("Other workspace")
	brief := &investigation.Brief{
		OriginIntegrationID: origin.ID.String(),
		OriginChannel:       "C-INCIDENT",
		OriginThread:        "1710000000.1",
	}

	offered := offeredSourcesForConversation(catalog,
		[]integrations.Integration{origin, other}, brief)
	if len(offered) != 1 || offered[0].Integration.ID != origin.ID {
		t.Fatalf("a mention must offer only its originating workspace: %+v", offered)
	}
	if len(offered[0].Tools) != 1 || offered[0].Tools[0].Name != "chat.thread" {
		t.Fatalf("a mention must offer only its originating thread read: %+v", offered[0].Tools)
	}
}
