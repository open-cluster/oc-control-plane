package agent

import (
	"sort"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
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
	// Delegated, never reimplemented. The same rule answers the operator surface's
	// Tool availability, and two copies of it would let what an operator is shown
	// disagree with what the investigator may actually call.
	return integrations.SupportedTools(definition, candidate)
}

// sortSourcesByName keeps the offer order stable run to run.
func sortSourcesByName(sources []investigation.OfferedSource) {
	sort.SliceStable(sources, func(i, j int) bool {
		return sources[i].Integration.Name < sources[j].Integration.Name
	})
}
