package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/investigation/agent"
	"github.com/open-cluster/oc-control-plane/internal/investigation/agent/providers"
)

// The rubric layer of issue #5 §11: a model grades what markers cannot — whether the
// conclusion names the cause as the cause — against the case's ground truth, for human
// spot-review. It runs only in real-model captures; the deterministic pipeline never
// pays for a judge.

// evalVerdict is one graded conclusion.
type evalVerdict struct {
	CausallyCorrect bool   `json:"causallyCorrect"`
	Actionability   int    `json:"actionability"`
	Note            string `json:"note"`
}

// judgeEvalCase asks the judge deployment to grade one record.
func judgeEvalCase(
	ctx context.Context, deployment reasoning.Deployment, one evalCase, record evalRecord,
) (*evalVerdict, error) {
	provider, err := providers.Open(deployment, providers.Options{})
	if err != nil {
		return nil, err
	}

	var truth strings.Builder
	truth.WriteString("GROUND TRUTH — the real causes in this scripted incident:\n")
	for _, cause := range one.Truth.Causes {
		truth.WriteString("- " + cause.Name + " (ground-truth markers: " +
			strings.Join(cause.Markers, ", ") + ")\n")
	}
	if len(one.Truth.Causes) == 0 {
		truth.WriteString("- none: the honest conclusion is that nothing was established\n")
	}
	for _, banned := range one.Truth.MustNotClaim {
		truth.WriteString("- a correct conclusion must NOT assert: " + banned + "\n")
	}
	truth.WriteString("\nTHE INVESTIGATION'S FINDINGS:\n")
	if len(record.Findings) == 0 {
		truth.WriteString("- none\n")
	}
	for _, finding := range record.Findings {
		fmt.Fprintf(&truth, "- %s (cites runs %v)\n", finding.Statement, finding.Sources)
	}
	truth.WriteString("\nGrade the findings against the ground truth. causallyCorrect " +
		"is true only when the real causes are identified and nothing banned is " +
		"asserted. actionability is 1-5: would an on-call engineer know what to fix?\n")

	completion, err := provider.Complete(ctx, reasoning.Prompt{
		Model: deployment.Model,
		System: []reasoning.Block{{Text: "You grade incident-investigation conclusions " +
			"against known ground truth. Answer only with the requested document.",
			Cache: true}},
		Content: []reasoning.Block{{Text: truth.String()}},
		Schema: reasoning.Schema{
			Name:    "judgement",
			Version: "1",
			Document: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"causallyCorrect": map[string]any{"type": "boolean"},
					"actionability":   map[string]any{"type": "integer"},
					"note":            map[string]any{"type": "string"},
				},
				"required":             []any{"actionability", "causallyCorrect", "note"},
				"additionalProperties": false,
			},
		},
		MaxOutputTokens: 2048,
		Effort:          reasoning.EffortLow,
	})
	if err != nil {
		return nil, err
	}

	verdict := &evalVerdict{}
	if err := json.Unmarshal(completion.Document, verdict); err != nil {
		return nil, fmt.Errorf("the judge's answer did not parse: %w", err)
	}
	return verdict, nil
}

// judgeTimeout bounds one grading call.
const judgeTimeout = 3 * time.Minute
