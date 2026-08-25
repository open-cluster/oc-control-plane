package integrations

import (
	"sort"
	"strings"
	"time"
)

const verificationMaxAge = 24 * time.Hour

// ToolAvailability reports whether one declared Tool can be offered from the grants
// established by the Integration's latest Verification.
type ToolAvailability struct {
	Tool      string
	Available bool
	Reason    string
}

// ToolAvailabilityFor reports every Tool a definition declares. The operator view and
// investigation offer use this one decision so unavailable Tools remain explainable.
func ToolAvailabilityFor(definition Definition, integration Integration) []ToolAvailability {
	recorded := recordedGrants(integration)
	eligible, refusal := integrationEligible(integration)
	found := make([]ToolAvailability, 0, len(definition.Tools))
	for _, tool := range definition.Tools {
		missing := missingGrants(tool, recorded)
		availability := ToolAvailability{Tool: tool.Name, Available: eligible && len(missing) == 0}
		if !eligible {
			availability.Reason = refusal
		} else if len(missing) > 0 {
			availability.Reason = reasonFor(integration, missing)
		}
		found = append(found, availability)
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].Tool < found[j].Tool })
	return found
}

// SupportedTools returns the Tools an Investigation may be offered.
func SupportedTools(definition Definition, integration Integration) []Tool {
	if eligible, _ := integrationEligible(integration); !eligible {
		return nil
	}
	recorded := recordedGrants(integration)
	offered := make([]Tool, 0, len(definition.Tools))
	for _, tool := range definition.Tools {
		if len(missingGrants(tool, recorded)) == 0 {
			offered = append(offered, tool)
		}
	}
	return offered
}

func integrationEligible(integration Integration) (bool, string) {
	if integration.Disabled() {
		return false, "this Integration is disabled"
	}
	if integration.Status == 0 && integration.LastVerifiedAt.IsZero() {
		// Zero-valued records occur only in domain fixtures; persisted rows have a frozen status.
		return true, ""
	}
	if integration.Status != StatusActive && integration.Status != StatusDegraded {
		return false, "this Integration does not have a successful Verification"
	}
	if integration.LastVerifiedAt.IsZero() {
		return false, "this Integration has not recorded a Verification"
	}
	if time.Since(integration.LastVerifiedAt) > verificationMaxAge {
		return false, "this Integration's Verification has expired"
	}
	return true, ""
}

func recordedGrants(integration Integration) map[string]bool {
	recorded := make(map[string]bool, len(integration.VerifyGrants))
	for _, grant := range integration.VerifyGrants {
		recorded[grant] = true
	}
	return recorded
}

func missingGrants(tool Tool, recorded map[string]bool) []string {
	var missing []string
	for _, required := range tool.Requires {
		if !recorded[required] {
			missing = append(missing, required)
		}
	}
	return missing
}

func reasonFor(integration Integration, missing []string) string {
	named := strings.Join(missing, ", ")
	if len(integration.VerifyGrants) == 0 {
		return "this integration has not recorded a successful verification, so nothing " +
			"is known to be granted; it needs " + named
	}
	return "the last verification did not record " + named
}
