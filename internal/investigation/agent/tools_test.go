package agent

import (
	"context"
	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"strings"
	"testing"
	"time"
)

func TestAnAnswerInsideTheBoundIsUntouched(t *testing.T) {
	t.Parallel()

	answer := "checkout-api is running v2.14.1."
	if got := boundedSummary(answer); got != answer {
		t.Errorf("boundedSummary(%q) = %q; a short summary must be left exactly alone",
			answer, got)
	}
}

func TestABoundedAnswerSaysItWasCut(t *testing.T) {
	t.Parallel()

	got := boundedSummary(strings.Repeat("a", investigation.MaxSummaryLength+500))

	if len([]rune(got)) > investigation.MaxSummaryLength {
		t.Errorf("boundedAnswer returned %d runes, past the bound of %d",
			len([]rune(got)), investigation.MaxSummaryLength)
	}
	if !strings.Contains(got, "truncated") {
		t.Errorf("a cut answer does not say it was cut, so it reads as a complete "+
			"one; tail = %q", tail(got))
	}
	if !strings.HasPrefix(got, "aaa") {
		t.Error("the answer's own words did not survive the cut")
	}
}

// The mark is only worth its characters if it is inside the bound: appending it after
// truncating to the ceiling would put the result back over.
func TestTheCutMarkIsInsideTheBound(t *testing.T) {
	t.Parallel()

	for _, over := range []int{1, 2, 500, investigation.MaxSummaryLength} {
		got := boundedSummary(strings.Repeat("b", investigation.MaxSummaryLength+over))
		if len([]rune(got)) > investigation.MaxSummaryLength {
			t.Errorf("over by %d: returned %d runes, past the bound of %d",
				over, len([]rune(got)), investigation.MaxSummaryLength)
		}
	}
}

func tail(text string) string {
	runes := []rune(text)
	if len(runes) <= 80 {
		return text
	}
	return string(runes[len(runes)-80:])
}

// A READ REPORTS A WINDOW ONLY WHEN IT USED ONE.
//
// Every run carries the window in force, because the record's column is NOT NULL and the
// bound is real. But a repository listing is not filtered by time, and an event that hands
// a reader a window beside it answers "did this read cover my period?" wrongly rather than
// not at all. Only a read that actually filtered by the window reports one.

func TestAnEventReportsNoWindowForAReadThatDidNotUseOne(t *testing.T) {
	t.Parallel()

	payload := investigation.ToolCompletedPayload(investigation.ToolRun{
		Ordinal: 1, Tool: "github.list_repositories", Outcome: investigation.RunSucceeded,
		Summary: "1 repositories matched",
		// The bound in force, as every run carries — but this read did not filter by it.
		WindowFrom:    time.Date(2026, 8, 21, 11, 0, 0, 0, time.UTC),
		WindowUntil:   time.Date(2026, 8, 22, 11, 0, 0, 0, time.UTC),
		WindowApplied: false,
	})

	if _, present := payload["windowFrom"]; present {
		t.Errorf("a listing that filtered by no window reports one: %v",
			payload["windowFrom"])
	}
}

func TestAnEventReportsTheWindowForAReadThatUsedOne(t *testing.T) {
	t.Parallel()

	from := time.Date(2026, 8, 21, 11, 0, 0, 0, time.UTC)
	payload := investigation.ToolCompletedPayload(investigation.ToolRun{
		Ordinal: 1, Tool: "github.read_commits", Outcome: investigation.RunSucceeded,
		Summary: "0 commits in the window", WindowFrom: from,
		WindowUntil:   time.Date(2026, 8, 22, 11, 0, 0, 0, time.UTC),
		WindowApplied: true,
	})

	if payload["windowFrom"] != from.Format(time.RFC3339) {
		t.Errorf("windowFrom = %v; a windowed read must say what it covered",
			payload["windowFrom"])
	}
}

// THE WINDOW A READ ACTUALLY COVERED.
//
// Every windowed read is clamped into the investigation's own window, including one the
// model phrased with no window at all. A model that is not told which window it got reads
// an empty result as a fact about the estate rather than about the bounds it was given —
// which is how "no commits in the last two hours" becomes "the repository has no commits".
// The rendered run states the window beside the arguments the model asked with, so a
// narrowing is visible by comparison.

func TestARunStatesTheWindowItActuallyCovered(t *testing.T) {
	t.Parallel()

	from := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	until := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)

	turn := renderResult(toolFeedback{
		CallID: "call-1",
		Run: investigation.ToolRun{
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

// A read that carries no window of its own — a repository listing, a pull request by
// number — must not grow a window line. Stating a window on a read that has none would
// tell the model its answer was bounded in time when it was not.
func TestARunWithNoWindowStatesNone(t *testing.T) {
	t.Parallel()

	turn := renderResult(toolFeedback{
		CallID: "call-1",
		Run: investigation.ToolRun{
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
