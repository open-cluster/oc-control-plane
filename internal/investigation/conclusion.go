package investigation

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"unicode/utf8"
)

//go:embed conclusion.schema.json
var conclusionSchema string

var ErrInvalidConclusion = errors.New("invalid Investigation Conclusion")

// ConclusionSchema returns the provider-facing schema for the canonical Conclusion.
func ConclusionSchema() json.RawMessage {
	return json.RawMessage(conclusionSchema)
}

// AssignedMessageConclusionSchema allows a factual reply without an assessment.
func AssignedMessageConclusionSchema() json.RawMessage {
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(ConclusionSchema(), &schema); err != nil {
		return ConclusionSchema()
	}
	schema["required"] = json.RawMessage(`["summary"]`)
	assigned, err := json.Marshal(schema)
	if err != nil {
		return ConclusionSchema()
	}
	return assigned
}

// ParseConclusion decodes an untrusted report into the canonical Conclusion.
func ParseConclusion(raw []byte) (Conclusion, error) {
	var conclusion Conclusion
	if len(raw) > MaxObservationBytes || !json.Valid(raw) {
		return conclusion, errors.New("the Conclusion must be one bounded JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&conclusion); err != nil {
		return Conclusion{}, errors.New("the Conclusion does not match the report schema")
	}
	if conclusion.Status == "" {
		var fields struct {
			Status json.RawMessage `json:"status"`
		}
		if err := json.Unmarshal(raw, &fields); err != nil || fields.Status != nil {
			return Conclusion{}, errors.New("the Conclusion assessment must be valid or omitted")
		}
	}
	if !boundedText(conclusion.Summary, MaxSummaryLength) {
		return Conclusion{}, errors.New("the Conclusion needs a bounded summary")
	}
	if utf8.RuneCountInString(conclusion.Impact.Summary) > MaxSummaryLength {
		return Conclusion{}, errors.New("the impact summary is too long")
	}
	conclusion.Findings = append([]Finding{}, conclusion.Findings...)
	for _, finding := range conclusion.Findings {
		if !slices.Contains(FindingKinds, string(finding.Kind)) ||
			!boundedText(finding.Statement, MaxSummaryLength) || utf8.RuneCountInString(finding.Mechanism) > MaxSummaryLength {
			return Conclusion{}, errors.New("each Finding needs a valid kind and bounded statement")
		}
	}
	conclusion.Hypotheses = append([]HypothesisResult{}, conclusion.Hypotheses...)
	for _, hypothesis := range conclusion.Hypotheses {
		if !slices.Contains(HypothesisStatuses, string(hypothesis.Status)) ||
			!boundedText(hypothesis.Statement, MaxSummaryLength) || !boundedText(hypothesis.Test, MaxSummaryLength) {
			return Conclusion{}, errors.New("each hypothesis needs a valid status, statement and test")
		}
	}
	if len(conclusion.Actions) > MaxConclusionActions {
		return Conclusion{}, errors.New("the Conclusion proposes too many actions")
	}
	conclusion.Actions = append([]ActionProposal{}, conclusion.Actions...)
	for _, action := range conclusion.Actions {
		for _, text := range []string{action.Title, action.Rationale, action.Verification} {
			if !boundedText(text, MaxActionTextLength) {
				return Conclusion{}, errors.New("each proposed action needs a bounded title, rationale and verification")
			}
		}
	}
	conclusion.Limitations = append([]Limitation{}, conclusion.Limitations...)
	for _, limitation := range conclusion.Limitations {
		if !slices.Contains(LimitationTypes, string(limitation.Type)) ||
			!boundedText(limitation.Statement, MaxSummaryLength) {
			return Conclusion{}, errors.New("each limitation needs a valid type and statement")
		}
	}
	return conclusion, nil
}

func boundedText(text string, limit int) bool {
	return strings.TrimSpace(text) != "" && utf8.RuneCountInString(text) <= limit
}

// ValidateConclusionStatus checks that the findings support the report's status.
// Durable evidence must also be validated by the store that commits the report.
func ValidateConclusionStatus(conclusion Conclusion, assignedMessage bool) error {
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
	case "":
		if !assignedMessage || hasExplanation {
			return errors.New("only a factual reply to an assigned Message can omit an assessment")
		}
	default:
		return errors.New("unknown Conclusion status")
	}
	return nil
}
