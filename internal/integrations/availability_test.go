package integrations

import (
	"strings"
	"testing"
	"time"
)

func definitionWithTools() Definition {
	return Definition{Tools: []Tool{
		{Name: "slack.list_channels"},
		{Name: "slack.search_messages", Requires: []string{"search:read"}},
	}}
}

func TestAvailabilityIsReportedPerToolWithoutAGenericCapabilityDeclaration(t *testing.T) {
	t.Parallel()

	found := ToolAvailabilityFor(definitionWithTools(),
		Integration{VerifyGrants: []string{"search:read"}})
	if len(found) != 2 {
		t.Fatalf("reported %d tools, want both declared tools: %+v", len(found), found)
	}
	if found[0].Tool == "" || found[1].Tool == "" {
		t.Fatalf("tool availability must identify each Tool: %+v", found)
	}
}

func TestAToolMissingItsGrantIsUnavailableAndSaysWhich(t *testing.T) {
	t.Parallel()

	found := ToolAvailabilityFor(definitionWithTools(), Integration{})
	for _, one := range found {
		if one.Tool != "slack.search_messages" {
			continue
		}
		if one.Available || one.Reason == "" {
			t.Fatalf("missing grant must be visible with a reason: %+v", one)
		}
		return
	}
	t.Fatal("search Tool was omitted")
}

func TestSupportedToolsUsesTheSameVerifiedGrantDecision(t *testing.T) {
	t.Parallel()

	without := SupportedTools(definitionWithTools(), Integration{})
	if len(without) != 1 || without[0].Name != "slack.list_channels" {
		t.Fatalf("ungranted Tool was offered: %+v", without)
	}
	with := SupportedTools(definitionWithTools(),
		Integration{VerifyGrants: []string{"search:read"}})
	if len(with) != 2 {
		t.Fatalf("verified grant did not offer both Tools: %+v", with)
	}
}

func TestToolAvailabilitySharesIntegrationEligibilityWithTheOffer(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	for _, scenario := range []struct {
		name        string
		integration Integration
		reason      string
		available   bool
	}{
		{"disabled", Integration{Status: StatusActive, LastVerifiedAt: now, DisabledAt: now}, "disabled", false},
		{"configured", Integration{Status: StatusConfigured, LastVerifiedAt: now}, "successful Verification", false},
		{"failed", Integration{Status: StatusFailed, LastVerifiedAt: now}, "successful Verification", false},
		{"unverified", Integration{Status: StatusActive}, "has not recorded a Verification", false},
		{"expired successful verification", Integration{Status: StatusActive, LastVerifiedAt: now.Add(-25 * time.Hour)}, "Verification has expired", false},
		{"recent successful verification", Integration{Status: StatusActive, LastVerifiedAt: now.Add(-23 * time.Hour)}, "", true},
		{"degraded current", Integration{Status: StatusDegraded, LastVerifiedAt: now}, "", true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			available := ToolAvailabilityFor(definitionWithTools(), scenario.integration)[0]
			if available.Available != scenario.available || !strings.Contains(available.Reason, scenario.reason) {
				t.Fatalf("availability=%+v, want available=%v reason=%q", available,
					scenario.available, scenario.reason)
			}
			if offered := SupportedTools(definitionWithTools(), scenario.integration); (len(offered) > 0) != scenario.available {
				t.Fatalf("offer and operator disagree: %+v / %+v", available, offered)
			}
		})
	}
}
