package agent

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

const SchemaVersion = "8"

type properties map[string]any

var (
	stringField  = map[string]any{"type": "string"}
	integerField = map[string]any{"type": "integer"}
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

func splitCalls(calls []CompletionCall) (reads []CompletionCall, conclude *CompletionCall) {
	for index, call := range calls {
		if call.Name == ConcludeToolName {
			if conclude == nil {
				conclude = &calls[index]
			}
			continue
		}
		reads = append(reads, call)
	}
	return reads, conclude
}

func decodeConclusion(document []byte, runs int, allowed []investigation.EvidenceRef,
) (investigation.Conclusion, error) {
	var jsonObject struct {
		Status  string `json:"status"`
		Summary string `json:"summary"`
		Impact  struct {
			Summary string `json:"summary"`
			RunRefs []int  `json:"run_refs"`
		} `json:"impact"`
		Findings []struct {
			Statement    string                      `json:"statement"`
			Kind         string                      `json:"kind"`
			Mechanism    string                      `json:"mechanism"`
			RunRefs      []int                       `json:"run_refs"`
			EvidenceRefs []investigation.EvidenceRef `json:"evidence_refs"`
		} `json:"findings"`
		Hypotheses []struct {
			ID,
			Statement,
			Status,
			Test string
			RunRefs []int `json:"run_refs"`
		} `json:"hypotheses"`
		Actions []struct {
			Title        string `json:"title"`
			Rationale    string `json:"rationale"`
			Verification string `json:"verification"`
			RunRefs      []int  `json:"run_refs"`
		} `json:"actions"`
		Limitations []struct {
			Type, Statement string
			RunRefs         []int `json:"run_refs"`
		} `json:"limitations"`
	}
	if err := json.Unmarshal(document, &jsonObject); err != nil {
		return investigation.Conclusion{}, fmt.Errorf(
			"the conclusion is not the declared document: %w", err)
	}

	if strings.TrimSpace(jsonObject.Summary) == "" {
		return investigation.Conclusion{}, fmt.Errorf(
			"the conclusion summary is empty")
	}
	if !oneOf(jsonObject.Status, investigation.ConclusionStatuses) {
		return investigation.Conclusion{}, fmt.Errorf("invalid conclusion status %q", jsonObject.Status)
	}
	if err := validateRunRefs(jsonObject.Impact.RunRefs, runs, "impact"); err != nil {
		return investigation.Conclusion{}, err
	}
	if strings.TrimSpace(jsonObject.Impact.Summary) == "" {
		return investigation.Conclusion{}, fmt.Errorf("the impact summary is empty")
	}
	conclusion := investigation.Conclusion{
		Status:  investigation.ConclusionStatus(jsonObject.Status),
		Summary: jsonObject.Summary,
		Impact: investigation.Impact{
			Summary: jsonObject.Impact.Summary,
			RunRefs: jsonObject.Impact.RunRefs,
		},
	}
	// Findings ------->
	for _, finding := range jsonObject.Findings {
		if finding.Statement == "" || len(finding.Statement) > maxStatementLength {
			return investigation.Conclusion{}, fmt.Errorf(
				"a finding's statement is empty or past %d characters", maxStatementLength)
		}
		if !oneOf(finding.Kind, investigation.GeneratedFindingKinds) {
			return investigation.Conclusion{}, fmt.Errorf(
				"a finding's kind %q is not in the declared vocabulary", finding.Kind)
		}
		if len(finding.RunRefs) == 0 && len(finding.EvidenceRefs) == 0 {
			return investigation.Conclusion{}, fmt.Errorf("a finding cites no run at all")
		}
		if err := validateRunRefs(finding.RunRefs, runs, "finding"); err != nil {
			return investigation.Conclusion{}, err
		}
		seenEvidence := make(map[investigation.EvidenceRef]bool, len(finding.EvidenceRefs))
		for _, ref := range finding.EvidenceRefs {
			if ref.InvestigationID == uuid.Nil || ref.ToolRunOrdinal <= 0 || !slices.Contains(allowed, ref) {
				return investigation.Conclusion{}, fmt.Errorf("a finding cites unavailable prior evidence")
			}
			if seenEvidence[ref] {
				return investigation.Conclusion{}, fmt.Errorf("a finding repeats prior evidence")
			}
			seenEvidence[ref] = true
		}
		if causalFinding(finding.Kind) && strings.TrimSpace(finding.Mechanism) == "" {
			return investigation.Conclusion{}, fmt.Errorf("causal finding has no mechanism")
		}
		conclusion.Findings = append(conclusion.Findings, investigation.Finding{
			Statement:    finding.Statement,
			Kind:         investigation.FindingKind(finding.Kind),
			Mechanism:    finding.Mechanism,
			RunRefs:      finding.RunRefs,
			EvidenceRefs: finding.EvidenceRefs,
		})
	}

	// Hypotheses ------->
	for _, hypothesis := range jsonObject.Hypotheses {
		if hypothesis.ID == "" || hypothesis.Statement == "" || hypothesis.Test == "" ||
			!oneOf(hypothesis.Status, investigation.HypothesisStatuses) {
			return investigation.Conclusion{}, fmt.Errorf("a hypothesis is incomplete or has an invalid status")
		}
		if err := validateRunRefs(hypothesis.RunRefs, runs, "hypothesis"); err != nil {
			return investigation.Conclusion{}, err
		}
		conclusion.Hypotheses = append(conclusion.Hypotheses, investigation.HypothesisResult{
			ID:        hypothesis.ID,
			Statement: hypothesis.Statement,
			Status:    investigation.HypothesisStatus(hypothesis.Status),
			Test:      hypothesis.Test,
			RunRefs:   hypothesis.RunRefs,
		})
	}
	if len(jsonObject.Actions) > investigation.MaxConclusionActions {
		return investigation.Conclusion{}, fmt.Errorf("the conclusion proposes %d actions, past %d",
			len(jsonObject.Actions), investigation.MaxConclusionActions)
	}

	// Actions ------->
	for _, action := range jsonObject.Actions {
		if action.Title == "" || action.Rationale == "" || action.Verification == "" ||
			len(action.Title) > investigation.MaxActionTextLength ||
			len(action.Rationale) > investigation.MaxActionTextLength ||
			len(action.Verification) > investigation.MaxActionTextLength {
			return investigation.Conclusion{}, fmt.Errorf(
				"an action is incomplete, invalid, or past %d characters", investigation.MaxActionTextLength)
		}
		if err := validateRunRefs(action.RunRefs, runs, "action"); err != nil {
			return investigation.Conclusion{}, err
		}
		if len(action.RunRefs) == 0 {
			return investigation.Conclusion{}, fmt.Errorf("action %q cites no run", action.Title)
		}
		conclusion.Actions = append(conclusion.Actions, investigation.ActionProposal{
			Title:        action.Title,
			Rationale:    action.Rationale,
			Verification: action.Verification,
			RunRefs:      action.RunRefs,
		})
	}

	// Limitations ------->
	for _, limitation := range jsonObject.Limitations {
		if limitation.Statement == "" || !oneOf(limitation.Type, investigation.LimitationTypes) {
			return investigation.Conclusion{}, fmt.Errorf("a limitation is incomplete or has an invalid type")
		}
		if err := validateRunRefs(limitation.RunRefs, runs, "limitation"); err != nil {
			return investigation.Conclusion{}, err
		}
		conclusion.Limitations = append(conclusion.Limitations, investigation.Limitation{
			Type:      investigation.LimitationType(limitation.Type),
			Statement: limitation.Statement,
			RunRefs:   limitation.RunRefs,
		})
	}
	if err := validateConclusionStatus(conclusion); err != nil {
		return investigation.Conclusion{}, err
	}
	return conclusion, nil
}

func validateRunRefs(refs []int, runs int, owner string) error {
	for _, ordinal := range refs {
		if ordinal < 1 || ordinal > runs {
			return fmt.Errorf("a %s cites run %d, and only %d ran", owner, ordinal, runs)
		}
	}
	return nil
}

func causalFinding(kind string) bool {
	return kind == string(investigation.FindingCause) ||
		kind == string(investigation.FindingContributingFactor)
}

func validateConclusionStatus(conclusion investigation.Conclusion) error {
	hasCause := false
	for _, finding := range conclusion.Findings {
		hasCause = hasCause || finding.Kind == investigation.FindingCause
	}
	switch conclusion.Status {
	case investigation.VerifiedCause:
		if !hasCause {
			return fmt.Errorf("verified_cause requires a cited cause with a mechanism")
		}
	case investigation.SupportedExplanation:
		explanation := false
		for _, finding := range conclusion.Findings {
			explanation = explanation || finding.Kind == investigation.FindingCause ||
				finding.Kind == investigation.FindingContributingFactor
		}
		if !explanation {
			return fmt.Errorf("supported_explanation requires a cited explanatory finding")
		}
		alternative := false
		for _, hypothesis := range conclusion.Hypotheses {
			alternative = alternative || hypothesis.Status == investigation.HypothesisExploring ||
				hypothesis.Status == investigation.HypothesisUnresolved
		}
		if !alternative {
			return fmt.Errorf("supported_explanation requires a plausible remaining alternative")
		}
	case investigation.Inconclusive:
		if hasCause {
			return fmt.Errorf("inconclusive cannot carry a cause")
		}
	case investigation.AnswerOnly:
		for _, finding := range conclusion.Findings {
			if finding.Kind != investigation.FindingObservation && finding.Kind != investigation.FindingRuledOut {
				return fmt.Errorf("answer_only cannot carry causal findings")
			}
		}
	}
	return nil
}

func oneOf(value string, allowed []string) bool {
	return slices.Contains(allowed, value)
}

const maxStatementLength = 2048
