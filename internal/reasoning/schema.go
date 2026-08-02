package reasoning

// THE OUTPUT SCHEMAS, AND WHAT THEY MAKE UNSTATEABLE.
//
// These are the reason there is no tool-use loop here and no tool definitions at all. The model is
// not being asked to DO anything; it is being asked to return a document, and a document is what
// the boundary already expects.
//
// The boundary's structural rules are enforced by absence rather than by instruction. A hypothesis
// reference is an integer ordinal among what the reasoner was shown; an evidence citation is an
// integer ordinal; a pod is an integer ordinal among the pods the brief actually resolved. There is
// no field anywhere below that accepts a stored identifier, a query, a selector, a path or a
// command, and the namespace, workload and time window a read runs under are filled in by this
// package from the case's own scope rather than offered to the model at all. A prompt-injected
// identifier therefore has nowhere to go: not because the model was told not to use one, but
// because there is no field it would fit in.
//
// The schemas use only what every provider's structured-output mode accepts: types, properties,
// required, enum, items and additionalProperties. Bounds — string lengths, array sizes, numeric
// ranges — are deliberately NOT expressed here, because several providers silently drop them and a
// bound that is silently dropped is a bound nobody has. They are enforced when the answer is
// decoded, where they hold for every provider equally.
//
// Every property is required. Optional fields are the one place two providers reliably disagree,
// so a field that does not apply carries an explicit empty value rather than being absent.

// SchemaVersion is the version of the three documents below. Bumping it invalidates every
// recording made against the old shape through the transcript key that already exists, which is
// the mechanism working rather than a cost.
const SchemaVersion = "3"

// Schemas is the three output contracts, one per method on the boundary.
func Schemas() (hypotheses, proposals, conclusion Schema) {
	return HypothesesSchema(), ProposalsSchema(), ConclusionSchema()
}

// HypothesesSchema is what the planner returns from the brief alone.
func HypothesesSchema() Schema {
	return Schema{
		Name:    "hypotheses",
		Version: SchemaVersion,
		Document: object(
			properties{
				"hypotheses": array(hypothesisSchema()),
			},
		),
	}
}

// hypothesisSchema is one proposed explanation. The same shape is offered at all three calls,
// because a cause the evidence revealed has to be sayable as a hypothesis rather than only inside a
// conclusion — a reasoner that could not propose one late would state it untethered or abstain on
// evidence naming the answer, and both are worse than letting it say what would disprove it.
func hypothesisSchema() map[string]any {
	return object(
		properties{
			// What might explain what was observed.
			"statement": stringField,
			// What would disprove it. It is required rather than optional because an explanation
			// nothing could disprove is a belief, and the whole apparatus under this exists to tell
			// those two apart.
			"falsifies": stringField,
		},
	)
}

// ProposalsSchema is what the planner returns when it is choosing further reads.
func ProposalsSchema() Schema {
	return Schema{
		Name:    "proposals",
		Version: SchemaVersion,
		Document: object(
			properties{
				"proposals": array(object(
					properties{
						"capability": enumField(
							kubernetesWorkloadRuntime,
							kubernetesNamespaceEvents,
							kubernetesContainerLogs,
						),
						// The one-based position of the hypothesis this read would support or
						// falsify, among those the planner was shown. It is an ordinal rather
						// than an identifier because a planner that could name a row could name
						// one it invented, and an invented identifier looks like a citation until
						// somebody follows it.
						"justification": integerField,
						"reason":        stringField,
						"arguments":     argumentsSchema(),
					},
				)),
				// Explanations this pass proposes that the brief alone could not have produced.
				// They are appended to the round and take the ordinals after the ones already held.
				"hypotheses": array(hypothesisSchema()),
				"weighings":  array(weighingSchema()),
				"settlings":  array(settlingSchema()),
			},
		),
	}
}

// ConclusionSchema is what the reasoner returns when it states an explanation or abstains.
func ConclusionSchema() Schema {
	return Schema{
		Name:    "conclusion",
		Version: SchemaVersion,
		Document: object(
			properties{
				// There is no fourth kind. A confident conclusion without sufficient support is
				// not a permitted outcome, and leaving it out of the enumeration is how that is
				// enforced rather than reviewed.
				"kind":      enumField("supported", "caveated", "abstained"),
				"statement": stringField,
				// The one-based position of the hypothesis this explanation IS, among those held
				// plus any proposed in this same document. Zero on an abstention. Without it a
				// round can falsify every alternative it proposed and still state a cause nobody
				// put forward, and the case file looks identical either way.
				"explains": integerField,
				// Explanations the evidence produced that nothing earlier proposed. They may be
				// settled by this same document and may be the one "explains" names.
				"hypotheses": array(hypothesisSchema()),
				"claims": array(object(
					properties{
						"role":      enumField("supporting", "contradicting", "affected_scope"),
						"statement": stringField,
						// One-based positions among the evidence items shown. Every claim carries
						// at least one, which is checked when this is decoded: an uncited claim
						// is impossible rather than discouraged.
						"evidence": array(integerField),
					},
				)),
				"unresolved":    array(integerField),
				"relevant_gaps": array(integerField),
				"weighings":     array(weighingSchema()),
				"settlings":     array(settlingSchema()),
			},
		),
	}
}

// argumentsSchema is the only shape a read can be asked for in.
//
// What is NOT here is the point. There is no namespace, no workload name, no time window and no
// pod name: the namespace, the workload and the window come from the case's own scope, and the pod
// is chosen by ordinal among the ones the brief resolved. A read is therefore structurally inside
// the investigation's scope before anything validates it — the model cannot express a read of
// another namespace, another workload, another time range or a pod nobody found.
func argumentsSchema() map[string]any {
	return object(
		properties{
			// The one-based position of the pod among the topology facts the brief listed. Zero
			// where the read is not about a particular pod.
			"pod": integerField,
			// The container within that pod. It is a name rather than an ordinal because the
			// brief does not enumerate containers; it carries no scope, because the pod it sits
			// inside is what scope is decided on, and the cluster refuses one that is not there.
			"container": stringField,
			// Whether to read the log of the container instance BEFORE the current one. It is
			// what answers "what did it say before it died", which is the single most decisive
			// artifact in a crashloop.
			"previous": booleanField,
			// Ceilings the read runs under. Zero asks for the capability's own ceiling.
			"max_pods":   integerField,
			"max_events": integerField,
			"max_lines":  integerField,
		},
	)
}

// weighingSchema is how one evidence item stands towards one hypothesis, both by ordinal.
func weighingSchema() map[string]any {
	return object(
		properties{
			"hypothesis": integerField,
			"evidence":   integerField,
			// Neutral is worth recording: what was considered and moved nothing is what shows a
			// hypothesis was examined rather than ignored.
			"stance": enumField("supports", "contradicts", "neutral"),
			"reason": stringField,
		},
	)
}

// settlingSchema moves one hypothesis, by ordinal, with the reason it moved.
func settlingSchema() map[string]any {
	return object(
		properties{
			"hypothesis": integerField,
			"state":      enumField("supported", "falsified", "set_aside"),
			// Required, because an alternative set aside silently is one a reader cannot check.
			"reason": stringField,
		},
	)
}

// The capability identifiers a read may name. They are the identifiers this build dispatches; a
// proposal naming anything else cannot be expressed, and is refused again by validation if a
// provider ignores the enumeration.
const (
	kubernetesWorkloadRuntime = "kubernetes.workload.runtime"
	kubernetesNamespaceEvents = "kubernetes.namespace.events"
	kubernetesContainerLogs   = "kubernetes.container.logs"
)

// The small JSON Schema vocabulary these documents are built from. It is a handful of helpers
// rather than a schema library because the whole surface is six keywords, and a library would
// invite the constraints that several providers silently drop.

type properties map[string]any

var (
	stringField  = map[string]any{"type": "string"}
	integerField = map[string]any{"type": "integer"}
	booleanField = map[string]any{"type": "boolean"}
)

func enumField(values ...string) map[string]any {
	allowed := make([]any, 0, len(values))
	for _, value := range values {
		allowed = append(allowed, value)
	}
	return map[string]any{"type": "string", "enum": allowed}
}

func array(items map[string]any) map[string]any {
	return map[string]any{"type": "array", "items": items}
}

// object closes the shape. additionalProperties is false everywhere and every property is
// required: a field a provider invented is refused, and a field it omitted is refused too.
func object(fields properties) map[string]any {
	required := make([]any, 0, len(fields))
	for name := range fields {
		required = append(required, name)
	}
	sortStrings(required)
	return map[string]any{
		"type":                 "object",
		"properties":           map[string]any(fields),
		"required":             required,
		"additionalProperties": false,
	}
}

// sortStrings orders the required list so a schema renders byte-identically every time. Map
// iteration order is random in Go, and a schema whose bytes moved between requests would
// invalidate a prompt cache for a reason nobody could see.
func sortStrings(values []any) {
	for outer := 1; outer < len(values); outer++ {
		for inner := outer; inner > 0; inner-- {
			left, leftOK := values[inner-1].(string)
			right, rightOK := values[inner].(string)
			if !leftOK || !rightOK || left <= right {
				break
			}
			values[inner-1], values[inner] = values[inner], values[inner-1]
		}
	}
}
