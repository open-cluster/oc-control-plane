package reasoning

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// The derived agent revision: attribution without a version anybody has to remember to
// bump. A manually maintained prompt-version constant is itself a drift source — the
// last one claimed pinning that never happened — so the revision is a hash over
// everything that shapes what the model sees: the frozen preamble, the conclusion
// schema, and the assembled tool-definition set. Any change to any of them changes the
// revision; nothing else does. It travels in per-call telemetry and evaluation records,
// and is deliberately not a schema column: no audit or replay consumer exists.

// AgentRevision derives this deployment's revision from the assembled catalog's tools.
// Computed once at startup, where the catalog is assembled.
func AgentRevision(tools []integrations.Tool) string {
	definitions := make([]integrations.ToolDefinition, 0, len(tools))
	for _, tool := range tools {
		definitions = append(definitions, tool.Definition())
	}
	return agentRevision(preamble, ConclusionSchema(), definitions)
}

// agentRevision hashes the three inputs. json.Marshal renders map keys sorted, so the
// same declarations hash identically across processes and builds.
func agentRevision(
	system string, conclusion Schema, definitions []integrations.ToolDefinition,
) string {
	hash := sha256.New()
	hash.Write([]byte(system))
	hash.Write([]byte{0})
	hash.Write(encodedForHash(conclusion))
	hash.Write([]byte{0})
	for _, definition := range definitions {
		hash.Write(encodedForHash(definition))
		hash.Write([]byte{0})
	}
	// Sixteen hex characters: enough to tell any two revisions apart in a dashboard,
	// short enough to read in a log line.
	return hex.EncodeToString(hash.Sum(nil))[:16]
}

// encodedForHash renders one input for hashing. An input JSON cannot express still
// perturbs the hash through its error text — silently skipping it would let two
// different inputs hash alike, which is the one thing this derivation must never do.
func encodedForHash(input any) []byte {
	encoded, err := json.Marshal(input)
	if err != nil {
		return []byte("unencodable: " + err.Error())
	}
	return encoded
}
