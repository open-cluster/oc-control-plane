package integrations

import (
	"context"
	"testing"
)

// What a capability report has to be right about: it is derived from VERIFIED reality, it
// says why something is unavailable rather than only that it is, and an optional permission
// nobody asked for is unavailable without that being an alarm.

func capabilityFixture() Definition {
	return Definition{
		ID: 99, Key: "stub", Name: "Stub", Category: CategoryAlerting,
		Capabilities: []string{"stub.list", "stub.search"},
		Probe: func(context.Context, ProbeInput) Verification {
			return Verification{Status: StatusActive}
		},
		Tools: []Tool{
			{
				Name: "stub.list", Capability: "stub.list", Description: "lists",
				WhenToUse: "listing", WhenNotToUse: "anything else",
				Permissions: "read", Output: "a list",
				Requires:    []string{"read"},
				Run: func(context.Context, ToolRequest) (ToolResult, error) {
					return ToolResult{}, nil
				},
			},
			{
				Name: "stub.search", Capability: "stub.search", Description: "searches",
				WhenToUse: "searching", WhenNotToUse: "anything else",
				Permissions: "search", Output: "hits",
				Requires:    []string{"search"},
				Run: func(context.Context, ToolRequest) (ToolResult, error) {
					return ToolResult{}, nil
				},
			},
		},
	}
}

func stateNamed(states []CapabilityState, name string) (CapabilityState, bool) {
	for _, state := range states {
		if state.Capability == name {
			return state, true
		}
	}
	return CapabilityState{}, false
}

func TestCapabilityStatesDeriveFromVerifiedGrants(t *testing.T) {
	t.Parallel()

	// The grant for listing is held and the grant for searching is not. That is the whole
	// input: availability is a fact about what the last verification recorded, never about
	// what a form accepted.
	states := capabilityFixture().CapabilityStatesFor(Integration{
		Type: 99, Status: StatusActive, VerifyGrants: []string{"read"},
	})

	listing, found := stateNamed(states, "stub.list")
	if !found {
		t.Fatalf("stub.list is not reported at all: %+v", states)
	}
	if !listing.Available {
		t.Errorf("stub.list has its grant and reads as unavailable: %+v", listing)
	}

	searching, found := stateNamed(states, "stub.search")
	if !found {
		t.Fatalf("stub.search is not reported at all: %+v", states)
	}
	if searching.Available {
		t.Errorf("stub.search lacks its grant and reads as available: %+v", searching)
	}
	if searching.Reason == "" {
		t.Error("an unavailable capability says nothing about why, which is the whole " +
			"point of reporting it rather than omitting it")
	}
}

func TestEveryDeclaredCapabilityIsReported(t *testing.T) {
	t.Parallel()

	// Reported individually means all of them. A console that receives only the working
	// ones cannot tell "unavailable" from "this build forgot".
	states := capabilityFixture().CapabilityStatesFor(Integration{
		Type: 99, Status: StatusActive,
	})
	if len(states) != 2 {
		t.Fatalf("2 declared capabilities reported as %d: %+v", len(states), states)
	}
	for _, state := range states {
		if state.Available {
			t.Errorf("%s reads as available with no grants recorded at all", state.Capability)
		}
	}
}

func TestAProviderMayJudgeItsOwnCapabilities(t *testing.T) {
	t.Parallel()

	// Some availability cannot be seen from the integration row: whether a deployment
	// registered an application with the vendor is deployment configuration, not a grant.
	// A provider that knows such a thing overrides the generic join.
	definition := capabilityFixture()
	definition.CapabilityStates = func(Integration) []CapabilityState {
		return []CapabilityState{{
			Capability: "stub.list", Available: false,
			Reason: "this deployment registered no application",
		}}
	}

	states := definition.CapabilityStatesFor(Integration{
		Type: 99, Status: StatusActive, VerifyGrants: []string{"read", "search"},
	})
	if len(states) != 1 || states[0].Available {
		t.Fatalf("the provider's own judgement was not used: %+v", states)
	}
}
