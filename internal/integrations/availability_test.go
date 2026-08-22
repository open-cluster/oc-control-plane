package integrations

import "testing"

// CAPABILITY AVAILABILITY, WITH THE REASON.
//
// Whether a capability is available is derived from ONE rule: a tool is usable only when
// every grant it requires was recorded by the last verification. That rule already decided
// which tools an investigation is offered. Serving it here is what stops a console
// recomputing the same question from parts and answering it differently — which it would,
// the first time a provider's grant vocabulary gains a member.
//
// The reason is half the point. "Unavailable" with no cause makes an operator guess whether
// they misconfigured something, chose not to grant it, or hit a bug.

func definitionWithTools() Definition {
	return Definition{
		Key:          "slack",
		Capabilities: []string{"slack.list_channels", "slack.search_messages"},
		Tools: []Tool{
			{Name: "slack.list_channels", Capability: "slack.list_channels"},
			{
				Name:       "slack.search_messages",
				Capability: "slack.search_messages",
				Requires:   []string{"search:read"},
			},
		},
	}
}

func TestACapabilityWithNoRequirementsIsAvailable(t *testing.T) {
	t.Parallel()

	found := Availability(definitionWithTools(), Integration{})

	got, ok := findCapability(found, "slack.list_channels")
	if !ok {
		t.Fatalf("the capability is missing from %+v", found)
	}
	if !got.Available {
		t.Errorf("a capability requiring no grant is unavailable: %+v", got)
	}
}

func TestACapabilityMissingItsGrantIsUnavailableAndSaysWhich(t *testing.T) {
	t.Parallel()

	found := Availability(definitionWithTools(), Integration{VerifyGrants: []string{"channels:read"}})

	got, ok := findCapability(found, "slack.search_messages")
	if !ok {
		t.Fatalf("the capability is missing from %+v", found)
	}
	if got.Available {
		t.Error("a capability whose required grant was never recorded reports available")
	}
	if got.Reason == "" {
		t.Error("unavailable with no reason makes an operator guess whether they " +
			"misconfigured it, declined it, or hit a bug")
	}
	if !containsText(got.Reason, "search:read") {
		t.Errorf("the reason does not name the missing grant: %q", got.Reason)
	}
}

func TestARecordedGrantMakesItsCapabilityAvailable(t *testing.T) {
	t.Parallel()

	found := Availability(definitionWithTools(),
		Integration{VerifyGrants: []string{"search:read"}})

	got, _ := findCapability(found, "slack.search_messages")
	if !got.Available {
		t.Errorf("a capability whose grant WAS recorded reports unavailable: %+v", got)
	}
	if got.Reason != "" {
		t.Errorf("an available capability carries a reason it does not need: %q", got.Reason)
	}
}

// Every declared capability is reported, present or not. A console rendering only what came
// back would silently omit a capability the deployment does declare.
func TestEveryDeclaredCapabilityIsReported(t *testing.T) {
	t.Parallel()

	found := Availability(definitionWithTools(), Integration{})
	if len(found) != 2 {
		t.Fatalf("reported %d capabilities, want both declared ones: %+v", len(found), found)
	}
}

func findCapability(all []CapabilityAvailability, name string) (CapabilityAvailability, bool) {
	for _, one := range all {
		if one.Capability == name {
			return one, true
		}
	}
	return CapabilityAvailability{}, false
}

func containsText(haystack, needle string) bool {
	return len(haystack) >= len(needle) && func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	}()
}

// A capability no TOOL exercises is not automatically broken. Alertmanager's whole job is
// receiving alerts inbound, and it does that through a webhook rather than a tool an
// investigation calls — so reporting it the way a missing grant is reported would tell an
// operator their working integration is unavailable. That is a new lie in the place a lie
// was being removed.
func TestAnInboundCapabilityIsNotReportedAsBroken(t *testing.T) {
	t.Parallel()

	inbound := Definition{
		Key:              "alertmanager",
		ReceivesWebhooks: true,
		Capabilities:     []string{"alertmanager.receive_alerts"},
	}
	got, ok := findCapability(Availability(inbound, Integration{}), "alertmanager.receive_alerts")
	if !ok {
		t.Fatal("the declared capability is not reported at all")
	}
	if !got.Available {
		t.Errorf("an inbound capability reports unavailable: %+v; an operator reads that "+
			"as a broken integration", got)
	}
	if !containsText(got.Reason, "inbound") && !containsText(got.Reason, "deliver") {
		t.Errorf("the reason does not say how it is exercised: %q", got.Reason)
	}
}

// A read-only type that declares a capability no tool implements IS a defect worth seeing:
// it is how an integration comes to advertise reads it cannot perform.
func TestADeclaredReadWithNoToolIsReportedUnavailable(t *testing.T) {
	t.Parallel()

	hollow := Definition{Key: "kubernetes", Capabilities: []string{"kubernetes.container.logs"}}
	got, _ := findCapability(Availability(hollow, Integration{}), "kubernetes.container.logs")
	if got.Available {
		t.Error("a capability with no tool behind it reports available")
	}
	if got.Reason == "" {
		t.Error("no reason given for a capability nothing implements")
	}
}
