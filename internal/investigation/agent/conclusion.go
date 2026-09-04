package agent

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

// SchemaVersion identifies the conclusion document shape.
const SchemaVersion = "6"

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

// object closes the shape so providers cannot add or omit fields.
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

// sortStrings keeps schema bytes stable for provider prompt caches.
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

// splitCalls separates the conclude call from the reads.
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

// decodeConclusion reads the conclude call against the conclusion contract and
// enforces the bounds the schema deliberately does not: citations must name runs that happened,
// kinds and confidences must be the declared vocabulary, and the texts must fit the
// record.
func decodeConclusion(
	document []byte, runs int, _ bool,
) (investigation.Conclusion, error) {
	var decoded struct {
		Status  string `json:"status"`
		Summary string `json:"summary"`
		Impact  struct {
			Status           string   `json:"status"`
			CurrentState     string   `json:"current_state"`
			AffectedServices []string `json:"affected_services"`
			AffectedUsers    []string `json:"affected_users"`
			Summary          string   `json:"summary"`
			RunRefs          []int    `json:"run_refs"`
		} `json:"impact"`
		Findings []struct {
			ID         string `json:"id"`
			Statement  string `json:"statement"`
			Kind       string `json:"kind"`
			Confidence string `json:"confidence"`
			Mechanism  string `json:"mechanism"`
			RunRefs    []int  `json:"run_refs"`
		} `json:"findings"`
		Hypotheses []struct {
			ID, Statement, Status, Test string
			RunRefs                     []int `json:"run_refs"`
		} `json:"hypotheses"`
		Actions []struct {
			Title            string `json:"title"`
			Type             string `json:"type"`
			Rationale        string `json:"rationale"`
			Risk             string `json:"risk"`
			Verification     string `json:"verification"`
			Reversible       bool   `json:"reversible"`
			RequiresApproval bool   `json:"requires_approval"`
			RunRefs          []int  `json:"run_refs"`
		} `json:"actions"`
		Limitations []struct {
			Type, Statement string
			RunRefs         []int `json:"run_refs"`
		} `json:"limitations"`
	}
	if err := json.Unmarshal(document, &decoded); err != nil {
		return investigation.Conclusion{}, fmt.Errorf(
			"the conclusion is not the declared document: %w", err)
	}

	if strings.TrimSpace(decoded.Summary) == "" {
		return investigation.Conclusion{}, fmt.Errorf(
			"the conclusion summary is empty")
	}
	if !oneOf(decoded.Status, investigation.ConclusionStatuses) {
		return investigation.Conclusion{}, fmt.Errorf("invalid conclusion status %q", decoded.Status)
	}
	if !oneOf(decoded.Impact.Status, investigation.ImpactStatuses) {
		return investigation.Conclusion{}, fmt.Errorf("invalid impact status %q", decoded.Impact.Status)
	}
	if err := validateRunRefs(decoded.Impact.RunRefs, runs, "impact"); err != nil {
		return investigation.Conclusion{}, err
	}
	if decoded.Impact.Status != string(investigation.ImpactUnknown) &&
		len(decoded.Impact.RunRefs) == 0 {
		return investigation.Conclusion{}, fmt.Errorf("known or partial impact cites no run")
	}
	if decoded.Impact.Status == string(investigation.ImpactUnknown) &&
		(len(decoded.Impact.AffectedServices) > 0 || len(decoded.Impact.AffectedUsers) > 0 ||
			len(decoded.Impact.RunRefs) > 0 || decoded.Impact.CurrentState != "unknown" ||
			!unknownImpactSummary(decoded.Impact.Summary)) {
		return investigation.Conclusion{}, fmt.Errorf("unknown impact cannot claim state, affected entities, or evidence")
	}
	conclusion := investigation.Conclusion{
		Status: investigation.ConclusionStatus(decoded.Status), Summary: decoded.Summary,
		Impact: investigation.ImpactAssessment{
			Status:       investigation.ImpactStatus(decoded.Impact.Status),
			CurrentState: decoded.Impact.CurrentState, AffectedServices: decoded.Impact.AffectedServices,
			AffectedUsers: decoded.Impact.AffectedUsers, Summary: decoded.Impact.Summary,
			RunRefs: decoded.Impact.RunRefs,
		},
	}
	for _, finding := range decoded.Findings {
		if finding.ID == "" || finding.Statement == "" || len(finding.Statement) > maxStatementLength {
			return investigation.Conclusion{}, fmt.Errorf(
				"a finding's id or statement is empty, or its statement is past %d characters", maxStatementLength)
		}
		if !oneOf(finding.Kind, investigation.FindingKinds) {
			return investigation.Conclusion{}, fmt.Errorf(
				"a finding's kind %q is not in the declared vocabulary", finding.Kind)
		}
		if !oneOf(finding.Confidence, investigation.Confidences) {
			return investigation.Conclusion{}, fmt.Errorf(
				"a finding's confidence %q is not confirmed, likely or possible",
				finding.Confidence)
		}
		if len(finding.RunRefs) == 0 {
			return investigation.Conclusion{}, fmt.Errorf("a finding cites no run at all")
		}
		if err := validateRunRefs(finding.RunRefs, runs, "finding"); err != nil {
			return investigation.Conclusion{}, err
		}
		if causalFinding(finding.Kind) && strings.TrimSpace(finding.Mechanism) == "" {
			return investigation.Conclusion{}, fmt.Errorf("causal finding %q has no mechanism", finding.ID)
		}
		conclusion.Findings = append(conclusion.Findings, investigation.Finding{
			ID: finding.ID, Statement: finding.Statement, Kind: finding.Kind,
			Confidence: finding.Confidence, Mechanism: finding.Mechanism, Sources: finding.RunRefs,
		})
	}
	for _, hypothesis := range decoded.Hypotheses {
		if hypothesis.ID == "" || hypothesis.Statement == "" || hypothesis.Test == "" ||
			!oneOf(hypothesis.Status, investigation.HypothesisStatuses) {
			return investigation.Conclusion{}, fmt.Errorf("a hypothesis is incomplete or has an invalid status")
		}
		if err := validateRunRefs(hypothesis.RunRefs, runs, "hypothesis"); err != nil {
			return investigation.Conclusion{}, err
		}
		conclusion.Hypotheses = append(conclusion.Hypotheses, investigation.HypothesisResult{
			ID: hypothesis.ID, Statement: hypothesis.Statement,
			Status: investigation.HypothesisStatus(hypothesis.Status), Test: hypothesis.Test,
			RunRefs: hypothesis.RunRefs,
		})
	}
	if len(decoded.Actions) > investigation.MaxConclusionActions {
		return investigation.Conclusion{}, fmt.Errorf("the conclusion proposes %d actions, past %d",
			len(decoded.Actions), investigation.MaxConclusionActions)
	}
	for _, action := range decoded.Actions {
		if action.Title == "" || action.Rationale == "" || action.Verification == "" ||
			len(action.Title) > investigation.MaxActionTextLength ||
			!oneOf(action.Type, investigation.ActionTypes) || !oneOf(action.Risk, investigation.ActionRisks) {
			return investigation.Conclusion{}, fmt.Errorf(
				"an action is incomplete, invalid, or past %d characters", investigation.MaxActionTextLength)
		}
		if err := validateRunRefs(action.RunRefs, runs, "action"); err != nil {
			return investigation.Conclusion{}, err
		}
		if len(action.RunRefs) == 0 {
			return investigation.Conclusion{}, fmt.Errorf("action %q cites no run", action.Title)
		}
		if stateChangingAction(action.Type) && !action.RequiresApproval {
			return investigation.Conclusion{}, fmt.Errorf("state-changing action %q does not require approval", action.Title)
		}
		conclusion.Actions = append(conclusion.Actions, investigation.ActionProposal{
			Title: action.Title, Type: investigation.ActionType(action.Type), Rationale: action.Rationale,
			Risk: investigation.ActionRisk(action.Risk), Reversible: action.Reversible,
			RequiresApproval: action.RequiresApproval, Verification: action.Verification,
			RunRefs: action.RunRefs,
		})
	}
	for _, limitation := range decoded.Limitations {
		if limitation.Statement == "" || !oneOf(limitation.Type, investigation.LimitationTypes) {
			return investigation.Conclusion{}, fmt.Errorf("a limitation is incomplete or has an invalid type")
		}
		if err := validateRunRefs(limitation.RunRefs, runs, "limitation"); err != nil {
			return investigation.Conclusion{}, err
		}
		conclusion.Limitations = append(conclusion.Limitations, investigation.Limitation{
			Type: investigation.LimitationType(limitation.Type), Statement: limitation.Statement,
			RunRefs: limitation.RunRefs,
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
	return kind == investigation.FindingCause || kind == investigation.FindingTrigger ||
		kind == investigation.FindingContributingFactor || kind == investigation.FindingPropagation
}

func stateChangingAction(kind string) bool {
	return kind == string(investigation.ActionMitigate) || kind == string(investigation.ActionRollback) ||
		kind == string(investigation.ActionFix)
}

func validateConclusionStatus(conclusion investigation.Conclusion) error {
	confirmedCause := false
	for _, finding := range conclusion.Findings {
		if finding.Kind == investigation.FindingCause && finding.Confidence == investigation.ConfidenceConfirmed {
			confirmedCause = true
		}
	}
	switch conclusion.Status {
	case investigation.VerifiedCause:
		if !confirmedCause {
			return fmt.Errorf("verified_cause requires a confirmed cited cause with a mechanism")
		}
	case investigation.SupportedExplanation:
		explanation := false
		for _, finding := range conclusion.Findings {
			explanation = explanation || finding.Kind == investigation.FindingCause ||
				finding.Kind == investigation.FindingTrigger ||
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
		if confirmedCause {
			return fmt.Errorf("inconclusive cannot carry a confirmed cause")
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

func unknownImpactSummary(summary string) bool {
	switch strings.ToLower(strings.TrimSpace(summary)) {
	case "impact is unknown.", "impact is not established.", "impact was not established.",
		"impact was not assessed for this operator question.":
		return true
	default:
		return false
	}
}

func oneOf(value string, allowed []string) bool {
	return slices.Contains(allowed, value)
}

// maxStatementLength bounds one finding. Enforced here rather than in the schema because
// several providers silently drop schema bounds.
const maxStatementLength = 2048
