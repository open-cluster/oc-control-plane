package investigation

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"unicode/utf8"
)

// ParseConclusion decodes an untrusted report into the canonical Conclusion.
func ParseConclusion(raw []byte) (Conclusion, error) {
	var conclusion Conclusion
	if len(raw) > MaxObservationBytes || !json.Valid(raw) {
		return conclusion, errors.New("the Conclusion must be one bounded JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&conclusion); err != nil {
		return Conclusion{}, errors.New("the Conclusion does not match the Tool schema")
	}
	if strings.TrimSpace(conclusion.Summary) == "" || utf8.RuneCountInString(conclusion.Summary) > MaxSummaryLength {
		return Conclusion{}, errors.New("the Conclusion needs a bounded summary")
	}
	if utf8.RuneCountInString(conclusion.Impact.Summary) > MaxSummaryLength ||
		(conclusion.Status != AnswerOnly && strings.TrimSpace(conclusion.Impact.Summary) == "") {
		return Conclusion{}, errors.New("describe the known or unknown impact in a bounded summary")
	}
	conclusion.Impact.RunRefs = append([]int{}, conclusion.Impact.RunRefs...)
	conclusion.Findings = append([]Finding{}, conclusion.Findings...)
	for i, finding := range conclusion.Findings {
		if !slices.Contains(FindingKinds, string(finding.Kind)) || strings.TrimSpace(finding.Statement) == "" || len(finding.Statement) > 2048 {
			return Conclusion{}, errors.New("each Finding needs a valid kind and bounded statement")
		}
		conclusion.Findings[i].RunRefs = append([]int{}, finding.RunRefs...)
	}
	conclusion.Hypotheses = append([]HypothesisResult{}, conclusion.Hypotheses...)
	for i, hypothesis := range conclusion.Hypotheses {
		if !slices.Contains(HypothesisStatuses, string(hypothesis.Status)) || strings.TrimSpace(hypothesis.Statement) == "" || strings.TrimSpace(hypothesis.Test) == "" {
			return Conclusion{}, errors.New("each hypothesis needs a valid status, statement and test")
		}
		conclusion.Hypotheses[i].RunRefs = append([]int{}, hypothesis.RunRefs...)
	}
	if len(conclusion.Actions) > MaxConclusionActions {
		return Conclusion{}, errors.New("the Conclusion proposes too many actions")
	}
	conclusion.Actions = append([]ActionProposal{}, conclusion.Actions...)
	for i, action := range conclusion.Actions {
		if len(action.EvidenceRefs) == 0 {
			return Conclusion{}, errors.New("each proposed action needs evidence")
		}
		for _, text := range []string{action.Title, action.Rationale, action.Verification} {
			if strings.TrimSpace(text) == "" || len(text) > MaxActionTextLength {
				return Conclusion{}, errors.New("each proposed action needs a bounded title, rationale and verification")
			}
		}
		conclusion.Actions[i].RunRefs = append([]int{}, action.RunRefs...)
	}
	conclusion.Limitations = append([]Limitation{}, conclusion.Limitations...)
	for i, limitation := range conclusion.Limitations {
		if !slices.Contains(LimitationTypes, string(limitation.Type)) || strings.TrimSpace(limitation.Statement) == "" || len(limitation.MessageSequences) != 0 {
			return Conclusion{}, errors.New("each limitation needs a valid type and statement")
		}
		conclusion.Limitations[i].RunRefs = append([]int{}, limitation.RunRefs...)
	}
	if err := ValidateConclusionStatus(conclusion); err != nil {
		return Conclusion{}, err
	}
	return conclusion, nil
}

// ValidateConclusionStatus checks that the findings support the report's status.
// Durable evidence must also be validated by the store that commits the report.
func ValidateConclusionStatus(conclusion Conclusion) error {
	var hasCause, hasExplanation bool
	for _, finding := range conclusion.Findings {
		causal := finding.Kind == FindingCause || finding.Kind == FindingContributingFactor
		if causal && strings.TrimSpace(finding.Mechanism) == "" {
			return errors.New("a causal Finding requires a mechanism")
		}
		hasCause = hasCause || finding.Kind == FindingCause
		hasExplanation = hasExplanation || causal
	}
	switch conclusion.Status {
	case SupportedExplanation:
		if !hasExplanation {
			return errors.New("supported_explanation requires a cited explanatory Finding")
		}
	case Inconclusive:
		if hasCause {
			return errors.New("inconclusive cannot carry a cause")
		}
	case AnswerOnly:
		if hasExplanation {
			return errors.New("answer_only cannot carry causal Findings")
		}
	default:
		return errors.New("unknown Conclusion status")
	}
	return nil
}
