package reasoning

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// The derived agent revision must move exactly when what the model sees moves: the
// preamble, the conclusion schema, or the tool-definition set — and must not move for
// anything else. That is the whole property a manually bumped version constant could
// never keep.

func revisionTool(name, description string) integrations.Tool {
	return integrations.Tool{
		Name: name, Capability: name, Description: description,
		WhenToUse: "when", WhenNotToUse: "when not", Permissions: "read",
		Output: "records",
		Arguments: []integrations.ToolArgument{
			{Name: "limit", Description: "how many", Type: integrations.FieldInteger},
		},
	}
}

// The preamble is pinned: an edit fails this test until the pin is updated, which is
// what makes every prompt change a deliberate act — the agent revision then moves on
// its own, attributing the change in telemetry and evaluation records.
func TestThePreambleIsPinned(t *testing.T) {
	t.Parallel()

	const pinned = "d0a403b9ca0ecf829de651816782500cc178d1595c46809672df77d9f1586fe7"
	digest := sha256.Sum256([]byte(preamble))
	if hex.EncodeToString(digest[:]) != pinned {
		t.Fatalf("the preamble changed. If the change is deliberate, update this pin to "+
			"%s; the derived agent revision has already moved with it.",
			hex.EncodeToString(digest[:]))
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
	base := agentRevision("the preamble", ConclusionSchema(),
		definitions(revisionTool("a.read", "reads a")))

	if moved := agentRevision("the preamble, edited", ConclusionSchema(),
		definitions(revisionTool("a.read", "reads a"))); moved == base {
		t.Error("an edited preamble did not move the revision")
	}
	otherSchema := ConclusionSchema()
	otherSchema.Version = "999"
	if moved := agentRevision("the preamble", otherSchema,
		definitions(revisionTool("a.read", "reads a"))); moved == base {
		t.Error("a changed conclusion schema did not move the revision")
	}
	if moved := agentRevision("the preamble", ConclusionSchema(),
		definitions(revisionTool("a.read", "reads a, differently"))); moved == base {
		t.Error("a changed tool description did not move the revision")
	}
	if moved := agentRevision("the preamble", ConclusionSchema(),
		definitions(revisionTool("a.read", "reads a"),
			revisionTool("b.read", "reads b"))); moved == base {
		t.Error("an added tool did not move the revision")
	}
}
