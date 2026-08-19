package investigation

import (
	"sort"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// The offer: which connected sources an investigation may read. Availability derives
// from each Integration's verified grants — fail closed, never a routed subset — and
// the investigator itself decides which offered sources to actually read.

// selection is one offered integration in the executor's shape.
type selection struct {
	integration integrations.Integration
	tools       []integrations.Tool
}

// offeredTools filters a definition's tools to those this integration's verified
// grants support. Fail closed: a tool whose Requires are not all recorded is absent
// from the investigation's set, never a call that always fails — which is how a pasted
// bot token stops being offered user-token-only search.
func offeredTools(
	definition integrations.Definition, candidate integrations.Integration,
) []integrations.Tool {
	recorded := make(map[string]bool, len(candidate.VerifyGrants))
	for _, grant := range candidate.VerifyGrants {
		recorded[grant] = true
	}
	offered := make([]integrations.Tool, 0, len(definition.Tools))
	for _, tool := range definition.Tools {
		supported := true
		for _, required := range tool.Requires {
			if !recorded[required] {
				supported = false
				break
			}
		}
		if supported {
			offered = append(offered, tool)
		}
	}
	return offered
}

// sortSourcesByName keeps the offer order stable run to run.
func sortSourcesByName(sources []OfferedSource) {
	sort.SliceStable(sources, func(i, j int) bool {
		return sources[i].Integration.Name < sources[j].Integration.Name
	})
}
