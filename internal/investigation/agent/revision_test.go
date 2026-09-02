package agent

import (
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// The derived agent revision must move exactly when what the model sees moves: the
// preamble, the conclusion schema, or the tool-definition set — and must not move for
// anything else. That is the whole property a manually bumped version constant could
// never keep. (The preamble's own pin lives beside the conversation tests.)

func revisionTool(name, description string) integrations.Tool {
	return integrations.Tool{
		Name: name, Description: description,
		WhenToUse: "when", WhenNotToUse: "when not", Permissions: "read",
		Output: "records",
		Arguments: []integrations.ToolArgument{
			{Name: "limit", Description: "how many", Type: integrations.FieldInteger},
		},
	}
}

func TestAgentRevisionIsStableForTheSameInputs(t *testing.T) {
	t.Parallel()

	tools := []integrations.Tool{revisionTool("a.read", "reads a")}
	first := AgentRevision(tools)
	if len(first) != 16 {
		t.Fatalf("revision %q is not 16 hex characters", first)
	}
	for range 8 {
		if again := AgentRevision(tools); again != first {
			t.Fatalf("two derivations over the same inputs differ: %q, %q", first, again)
		}
	}
}

func TestAgentRevisionMovesWithEachOfItsThreeInputs(t *testing.T) {
	t.Parallel()

	definitions := func(tools ...integrations.Tool) []integrations.ToolDefinition {
		rendered := make([]integrations.ToolDefinition, 0, len(tools))
		for _, tool := range tools {
			rendered = append(rendered, tool.Definition())
		}
		return rendered
	}
	base := agentRevision("the preamble", concludeSchema(),
		definitions(revisionTool("a.read", "reads a")))

	if moved := agentRevision("the preamble, edited", concludeSchema(),
		definitions(revisionTool("a.read", "reads a"))); moved == base {
		t.Error("an edited preamble did not move the revision")
	}
	otherSchema := concludeSchema()
	otherSchema.Version = "999"
	if moved := agentRevision("the preamble", otherSchema,
		definitions(revisionTool("a.read", "reads a"))); moved == base {
		t.Error("a changed conclusion schema did not move the revision")
	}
	if moved := agentRevision("the preamble", concludeSchema(),
		definitions(revisionTool("a.read", "reads a, differently"))); moved == base {
		t.Error("a changed tool description did not move the revision")
	}
	if moved := agentRevision("the preamble", concludeSchema(),
		definitions(revisionTool("a.read", "reads a"),
			revisionTool("b.read", "reads b"))); moved == base {
		t.Error("an added tool did not move the revision")
	}
}
