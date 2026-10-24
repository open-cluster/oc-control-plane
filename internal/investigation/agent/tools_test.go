package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

func TestConclusionSummaryIsValidatedWithoutTruncation(t *testing.T) {
	t.Parallel()
	for _, answer := range []string{"checkout-api is running v2.14.1.", strings.Repeat("界", 4096), strings.Repeat("界", 4097)} {
		var document map[string]any
		if err := json.Unmarshal(validConclusion(t, nil), &document); err != nil {
			t.Fatal(err)
		}
		document["summary"] = answer
		encoded, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		conclusion, err := decodeConclusion(encoded, nil, nil)
		if len([]rune(answer)) > 4096 {
			if err == nil {
				t.Fatal("an oversized summary was accepted instead of requesting correction")
			}
		} else if err != nil || conclusion.Summary != answer {
			t.Fatalf("a valid summary changed or was rejected: %v", err)
		}
	}
}

func TestExternalToolCallsCarryPurposeAndInputWithoutAHypothesisAssociation(t *testing.T) {
	definition := envelopeDefinition(integrations.ToolDefinition{
		Name:        "github.read_commits",
		InputSchema: map[string]any{"type": "object"},
	})

	properties, ok := definition.InputSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties = %#v", definition.InputSchema["properties"])
	}
	if _, present := properties["hypothesisId"]; present {
		t.Errorf("external Tool call still associates a Tool Run with a hypothesis: %#v", properties)
	}
	if properties["purpose"] == nil || properties["input"] == nil {
		t.Errorf("purpose or input was removed with the hypothesis association: %#v", properties)
	}
}

func TestARunStatesTheWindowItActuallyCovered(t *testing.T) {
	t.Parallel()

	from := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	until := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)

	turn := renderResult(toolFeedback{
		CallID: "call-1",
		ToolRun: investigation.ToolRun{
			Ordinal:       1,
			Tool:          "github.read_commits",
			Arguments:     map[string]any{"repositoryId": 42},
			Outcome:       investigation.RunSucceeded,
			Summary:       "0 commits",
			WindowFrom:    from,
			WindowUntil:   until,
			WindowApplied: true,
			Content:       []any{},
		},
	})

	if !strings.Contains(turn.Content, stamp(from)) ||
		!strings.Contains(turn.Content, stamp(until)) {
		t.Errorf("the run does not say which window it covered:\n%s", turn.Content)
	}
	if !strings.Contains(turn.Content, "WINDOW:") {
		t.Errorf("the window is not labelled, so it reads as an arbitrary pair of "+
			"timestamps:\n%s", turn.Content)
	}
}

func TestARunWithNoWindowStatesNone(t *testing.T) {
	t.Parallel()

	turn := renderResult(toolFeedback{
		CallID: "call-1",
		ToolRun: investigation.ToolRun{
			Ordinal:   1,
			Tool:      "github.list_repositories",
			Arguments: map[string]any{},
			Outcome:   investigation.RunSucceeded,
			Summary:   "1 repositories matched",
			Content:   []any{},
		},
	})

	if strings.Contains(turn.Content, "WINDOW:") {
		t.Errorf("a read with no window claims one:\n%s", turn.Content)
	}
}

func stubIntegration(name string) integrations.Integration {
	return integrations.Integration{ID: uuid.New(), Provider: "stub", Name: name,
		Status: integrations.StatusVerified, VerifiedAt: time.Now().UTC()}
}

func TestTheOfferRequiresCurrentVerificationAndKeepsSameTypeSourcesReachable(t *testing.T) {
	t.Parallel()

	definition := integrations.Definition{
		Manifest: integrations.Manifest{Key: "stub", Name: "Stub",
			Category: integrations.CategoryAlerting,
			Tools: []integrations.Tool{{
				Name: "stub.read", Description: "Read items when asked about them.",
				Run: func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
					return integrations.ToolResult{}, nil
				},
			}}},
		Probe: func(context.Context, integrations.ProbeInput) integrations.Verification {
			return integrations.Verification{Status: integrations.StatusVerified}
		},
	}
	catalog, err := integrations.NewCatalog(definition)
	if err != nil {
		t.Fatal(err)
	}
	first := stubIntegration("First")
	first.Status, first.VerifiedAt = integrations.StatusVerified, time.Now().UTC()
	second := stubIntegration("Second")
	configured := stubIntegration("Configured")
	configured.Status, configured.VerifiedAt = "", time.Time{}
	stale := stubIntegration("Stale")
	stale.Status, stale.VerifiedAt = integrations.StatusVerified, time.Now().Add(-30*24*time.Hour)

	offered := offeredSources(catalog, []integrations.Integration{first, second, configured, stale})
	if len(offered) != 3 {
		t.Fatalf("offered %d sources, want every successfully verified source", len(offered))
	}
	selected := offered
	seen := map[string]bool{}
	for _, source := range selected {
		if len(source.Tools) != 1 || !strings.Contains(source.Tools[0].Name, "__") {
			t.Fatalf("same-type Tool was not Integration-bound: %+v", source.Tools)
		}
		resolved, _, ok := toolNamed(selected, source.Tools[0].Name)
		if !ok || resolved.Integration.ID != source.Integration.ID {
			t.Fatalf("Tool %q did not resolve to Integration %s", source.Tools[0].Name,
				source.Integration.ID)
		}
		seen[source.Integration.ID.String()] = true
	}
	if !seen[first.ID.String()] || !seen[second.ID.String()] || !seen[stale.ID.String()] {
		t.Fatalf("same-type Integrations were not both reachable: %v", seen)
	}
	if _, _, ok := toolNamed(selected, "stub.read"); ok {
		t.Fatal("an ambiguous bare Tool name must not resolve to either Integration")
	}
}

func TestTheOfferHoldsOnlyToolsTheVerifiedGrantsSupport(t *testing.T) {
	t.Parallel()

	catalog, err := integrations.NewCatalog(integrations.Definition{
		Manifest: integrations.Manifest{Key: "stub", Name: "Stub",
			Category: integrations.CategoryAlerting,
			Tools: []integrations.Tool{
				{
					Name: "stub.read", Description: "Read items.",
					Run: func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
						return integrations.ToolResult{}, nil
					},
				},
				{
					Name: "stub.search", Description: "Search items when their identity is unknown.",
					RequiredGrants: []string{"search:read", "user_token"},
					Run: func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
						return integrations.ToolResult{}, nil
					},
				},
			}},
		Probe: func(context.Context, integrations.ProbeInput) integrations.Verification {
			return integrations.Verification{Status: integrations.StatusVerified}
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	granted := stubIntegration("Fully Granted")
	granted.VerificationGrants = []string{"search:read", "user_token", "channels:read"}
	partial := stubIntegration("Bot Token")
	partial.VerificationGrants = []string{"search:read"}
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
		Manifest: integrations.Manifest{Key: "gated", Name: "Gated",
			Category: integrations.CategoryAlerting,
			Tools: []integrations.Tool{{
				Name: "gated.search", Description: "Search items with a user token.",
				RequiredGrants: []string{"user_token"},
				Run: func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
					return integrations.ToolResult{}, nil
				},
			}}},
		Probe: func(context.Context, integrations.ProbeInput) integrations.Verification {
			return integrations.Verification{Status: integrations.StatusVerified}
		},
	}
	gatedCatalog, err := integrations.NewCatalog(searchOnly)
	if err != nil {
		t.Fatal(err)
	}
	botOnly := integrations.Integration{Provider: "gated", Name: "Bot Only"}
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
		Manifest: integrations.Manifest{Key: "chat", Name: "Chat",
			Category: integrations.CategoryAlerting,
			Tools: []integrations.Tool{
				{
					Name: "chat.thread", Description: "Read the originating thread.",
					SupportsThreadScope: true, Run: read,
				},
				{
					Name: "chat.channel", Description: "Read an entire channel when explicitly granted.",
					Run: read,
				},
			}},
		Probe: func(context.Context, integrations.ProbeInput) integrations.Verification {
			return integrations.Verification{Status: integrations.StatusVerified}
		},
	}
	catalog, err := integrations.NewCatalog(definition)
	if err != nil {
		t.Fatal(err)
	}
	origin := stubIntegration("Origin workspace")
	other := stubIntegration("Other workspace")
	origin.Provider = "chat"
	other.Provider = "chat"
	scope := &investigation.ConversationOrigin{
		IntegrationID: origin.ID,
		Channel:       "C-INCIDENT",
		Thread:        "1710000000.1",
	}

	offered := offeredSourcesForConversation(catalog,
		[]integrations.Integration{origin, other}, scope)
	if len(offered) != 1 || offered[0].Integration.ID != origin.ID {
		t.Fatalf("a mention must offer only its originating workspace: %+v", offered)
	}
	if len(offered[0].Tools) != 1 || offered[0].Tools[0].Name != "chat.thread" {
		t.Fatalf("a mention must offer only its originating thread read: %+v", offered[0].Tools)
	}
}
