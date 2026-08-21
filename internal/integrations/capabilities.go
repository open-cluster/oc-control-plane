package integrations

import "strings"

// WHAT AN INTEGRATION CAN ACTUALLY DO, ANSWERED HERE RATHER THAN GUESSED IN A BROWSER.
//
// A type declares the capabilities connecting it makes available. An Integration records
// the grants its last verification established. Whether a particular capability works is
// the join of the two — and until now that join happened in a console, which meant every
// client reimplemented it and none of them could see the half that is not in the row.
//
// It is served with a REASON, because "unavailable" alone cannot distinguish the three
// things an operator would do differently about: a permission that was revoked, a
// permission this product deliberately never requests, and a capability this deployment is
// not configured to offer at all. The first is a problem to fix, the second is a decision
// to understand, and the third is a deployment to configure.
//
// This is not a capability framework. It is the grants mechanism that already gates tools,
// read at the capability a tool declares, so a capability cannot claim to work when every
// tool exercising it is absent from the offered set.

// CapabilityState is one declared capability judged against verified reality.
type CapabilityState struct {
	// Capability is the declared name, as the type's catalog entry lists it.
	Capability string
	Available  bool
	// Reason says why an unavailable capability is unavailable, in the operator's
	// language. Empty when it is available: there is nothing to explain about a thing
	// that works.
	Reason string
}

// CapabilityStatesFor reports every capability this definition declares, each judged
// against what the Integration's last verification recorded.
//
// EVERY capability, always, in declared order. A report that omitted the ones that do not
// work would be one a reader cannot tell from a report this build forgot to make, and the
// deliberate absences — the permissions this product declines to hold — are exactly the
// ones worth saying out loud.
func (d Definition) CapabilityStatesFor(integration Integration) []CapabilityState {
	if d.CapabilityStates != nil {
		// The provider knows something the row does not. Whether this deployment
		// registered an application with the vendor is configuration rather than a
		// grant, and no join over Integration fields can see it.
		return d.CapabilityStates(integration)
	}

	held := make(map[string]bool, len(integration.VerifyGrants))
	for _, grant := range integration.VerifyGrants {
		held[grant] = true
	}

	states := make([]CapabilityState, 0, len(d.Capabilities))
	for _, capability := range d.Capabilities {
		states = append(states, d.judge(capability, held))
	}
	return states
}

// judge decides one capability from the tools that exercise it.
//
// Available when SOME tool exercising it can be offered. A capability served by two tools
// where one needs a grant the credential lacks is still a capability the integration has,
// and reporting it as broken because the richer path is shut would be reporting the wrong
// thing.
func (d Definition) judge(capability string, held map[string]bool) CapabilityState {
	var missing []string
	exercised := false

	for _, tool := range d.Tools {
		if tool.Capability != capability {
			continue
		}
		exercised = true
		lacking := lacked(tool.Requires, held)
		if len(lacking) == 0 {
			return CapabilityState{Capability: capability, Available: true}
		}
		missing = append(missing, lacking...)
	}

	if !exercised {
		// Nothing gates it. A capability no tool exercises is one this join has no
		// evidence against, and inventing a refusal would be inventing a fact.
		return CapabilityState{Capability: capability, Available: true}
	}
	return CapabilityState{
		Capability: capability,
		Reason: "the last verification did not record " + strings.Join(unique(missing), " or "),
	}
}

// lacked reports which of a tool's required grants are not held.
func lacked(required []string, held map[string]bool) []string {
	var lacking []string
	for _, grant := range required {
		if !held[grant] {
			lacking = append(lacking, grant)
		}
	}
	return lacking
}

// unique keeps the first mention of each grant, so a capability served by two tools that
// need the same thing does not name it twice.
func unique(grants []string) []string {
	seen := make(map[string]bool, len(grants))
	kept := make([]string, 0, len(grants))
	for _, grant := range grants {
		if seen[grant] {
			continue
		}
		seen[grant] = true
		kept = append(kept, grant)
	}
	return kept
}
