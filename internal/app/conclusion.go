package app

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

func addConclusionTool(profile *ExecutionProfile, commit func(context.Context, investigation.Conclusion) error) {
	const name = "conclude"
	finalize := tool.NewFunctionTool(name, "Commit the final Investigation report. Call this Tool alone.",
		json.RawMessage(conclusionSchema), func(ctx context.Context, input map[string]any) (any, error) {
			raw, err := json.Marshal(input)
			if err != nil {
				return nil, errors.New("could not read the Conclusion")
			}
			conclusion, err := investigation.ParseConclusion(raw)
			if err != nil {
				return nil, err
			}
			if err := commit(ctx, conclusion); err != nil {
				return nil, err
			}
			return "Conclusion accepted.", nil
		})
	// The Tool records a report in OpenCluster and has no action in a connected source.
	finalize.ReadOnly = true
	if profile.Toolkit == nil {
		profile.Toolkit = tool.NewToolkit()
	}
	profile.Toolkit.AddGroup(name, finalize)
	profile.ExclusiveTools = []string{name}
}

const conclusionSchema = `{
  "type": "object",
  "$defs": {"citations": {"type": "array", "items": {"type": "object",
    "required": ["toolRunId"], "properties": {"toolRunId": {"type": "string", "format": "uuid"}}}}},
  "required": ["status", "summary", "impact", "findings", "hypotheses", "actions", "limitations"],
  "properties": {
    "status": {"type": "string", "enum": ["supported_explanation", "inconclusive", "answer_only"]},
    "summary": {"type": "string", "maxLength": 4096},
    "impact": {"type": "object", "required": ["summary"], "properties": {
      "summary": {"type": "string"}, "evidenceRefs": {"$ref": "#/$defs/citations"}}},
    "findings": {"type": "array", "items": {"type": "object",
      "required": ["statement", "kind", "mechanism", "evidenceRefs"],
      "properties": {
        "statement": {"type": "string"},
        "kind": {"type": "string", "enum": ["cause", "contributing_factor", "ruled_out", "observation"]},
        "mechanism": {"type": "string"},
        "evidenceRefs": {"$ref": "#/$defs/citations", "minItems": 1}
      }}},
    "hypotheses": {"type": "array", "items": {"type": "object",
      "required": ["statement", "status", "test"], "properties": {
        "statement": {"type": "string"}, "test": {"type": "string"},
        "status": {"type": "string", "enum": ["supported", "ruled_out", "unresolved"]},
        "evidenceRefs": {"$ref": "#/$defs/citations"}
      }}},
    "actions": {"type": "array", "maxItems": 8, "items": {"type": "object",
      "required": ["title", "rationale", "verification", "evidenceRefs"], "properties": {
        "title": {"type": "string"}, "rationale": {"type": "string"}, "verification": {"type": "string"},
        "evidenceRefs": {"$ref": "#/$defs/citations", "minItems": 1}
      }}},
    "limitations": {"type": "array", "items": {"type": "object",
      "required": ["type", "statement"], "properties": {
        "type": {"type": "string", "enum": ["missing_telemetry", "missing_access", "contradiction", "unresolved_assumption", "essential_human_input"]},
        "statement": {"type": "string"}, "evidenceRefs": {"$ref": "#/$defs/citations"}
      }}}
  }
}`
