package integrations

import (
	"sort"
	"strings"
)

// AVAILABILITY IS ONE RULE, DERIVED HERE AND NOWHERE ELSE.
//
// A tool is usable only when every grant it requires was recorded by the last
// verification. That rule already decides which tools an investigation is offered; it
// lives here, below both consumers, so the answer an operator is shown and the answer the
// investigator acts on cannot disagree. Recomputing it anywhere else — most temptingly in
// a browser, by joining a type's declared capabilities against an integration's grants —
// is an integration's status assembled from parts, free to diverge the first time a
// provider's grant vocabulary gains a member.

// CapabilityAvailability is one declared capability and whether this integration can
// actually exercise it.
//
// Reason is empty for the ordinary case — a read that works. It is present in the two
// cases a caller would otherwise have to guess at: when the capability cannot be used, it
// names what is missing, because unavailable with no cause makes an operator guess whether
// they misconfigured something, deliberately declined it, or hit a bug; and when the
// capability is available but is not a read an investigation calls, it says how it IS
// exercised, so an inbound-only integration is not rendered as a set of tools nobody can
// invoke.
type CapabilityAvailability struct {
	Capability string
	Available  bool
	Reason     string
}

// Availability reports every capability the definition declares, with whether this
// integration's recorded grants support it. Every declared capability is reported, present
// or absent — a caller rendering only what came back would silently omit one.
func Availability(
	definition Definition, integration Integration,
) []CapabilityAvailability {
	recorded := recordedGrants(integration)

	// A capability is exercised by the tools that declare it. It is available when at
	// least one of them is, because that is exactly what the investigation is offered.
	best := make(map[string]CapabilityAvailability, len(definition.Capabilities))
	for _, tool := range definition.Tools {
		missing := missingGrants(tool, recorded)
		candidate := CapabilityAvailability{
			Capability: tool.Capability,
			Available:  len(missing) == 0,
		}
		if !candidate.Available {
			candidate.Reason = reasonFor(integration, missing)
		}
		if held, seen := best[tool.Capability]; seen && (held.Available || !candidate.Available) {
			continue
		}
		best[tool.Capability] = candidate
	}

	found := make([]CapabilityAvailability, 0, len(definition.Capabilities))
	for _, capability := range definition.Capabilities {
		if one, ok := best[capability]; ok {
			found = append(found, one)
			continue
		}
		// Declared but exercised by no tool, which means two opposite things.
		//
		// On a type reached INBOUND, it is how the type works: Alertmanager's whole job is
		// receiving alerts through its webhook, and no investigation ever calls it. Marking
		// that unavailable would tell an operator their working integration is broken.
		//
		// On a type that is only ever read FROM, it is a defect worth seeing — it is how an
		// integration comes to advertise reads it cannot perform.
		if definition.ReceivesWebhooks {
			found = append(found, CapabilityAvailability{
				Capability: capability,
				Available:  true,
				Reason:     "delivered inbound to this integration, not called as a read",
			})
			continue
		}
		found = append(found, CapabilityAvailability{
			Capability: capability,
			Reason:     "this build ships no tool that exercises it",
		})
	}
	sort.SliceStable(found, func(i, j int) bool {
		return found[i].Capability < found[j].Capability
	})
	return found
}

// SupportedTools is the same rule, returning the tools an investigation may be offered.
func SupportedTools(definition Definition, integration Integration) []Tool {
	recorded := recordedGrants(integration)
	offered := make([]Tool, 0, len(definition.Tools))
	for _, tool := range definition.Tools {
		if len(missingGrants(tool, recorded)) == 0 {
			offered = append(offered, tool)
		}
	}
	return offered
}

func recordedGrants(integration Integration) map[string]bool {
	recorded := make(map[string]bool, len(integration.VerifyGrants))
	for _, grant := range integration.VerifyGrants {
		recorded[grant] = true
	}
	return recorded
}

// missingGrants names what this tool needs and the verification did not record. Order
// follows the tool's own declaration, so the reason reads the way the contract is written.
func missingGrants(tool Tool, recorded map[string]bool) []string {
	var missing []string
	for _, required := range tool.Requires {
		if !recorded[required] {
			missing = append(missing, required)
		}
	}
	return missing
}

// reasonFor states why a capability is unavailable, distinguishing the two cases an
// operator acts on differently: nothing has been verified yet, or verification happened
// and the credential did not carry this.
func reasonFor(integration Integration, missing []string) string {
	named := strings.Join(missing, ", ")
	if len(integration.VerifyGrants) == 0 {
		return "this integration has not recorded a successful verification, so nothing " +
			"is known to be granted; it needs " + named
	}
	return "the last verification did not record " + named
}
