package investigation

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// What this surface says on the wire. Kept apart from the handlers because it is a
// contract: a field renamed here is a client broken somewhere else.

type errorView struct {
	Error string `json:"error"`
}

// clarificationView is the answer to a question that could not be tied to one incident:
// one plain-language question back, and nothing opened.
type clarificationView struct {
	Clarification string `json:"clarification"`
}

type findingView struct {
	ID        string `json:"id"`
	Statement string `json:"statement"`
	// Kind is the finding's causal role and Confidence its categorical certainty.
	// Absent on findings concluded before the vocabulary existed.
	Kind       string `json:"kind,omitempty"`
	Confidence string `json:"confidence,omitempty"`
	Mechanism  string `json:"mechanism,omitempty"`
	// Sources are one-based ordinals among the investigation's runs.
	RunRefs []int `json:"runRefs"`
}

type spendView struct {
	InputTokens  int64 `json:"inputTokens"`
	OutputTokens int64 `json:"outputTokens"`
	MicroCents   int64 `json:"microCents"`
}

type investigationView struct {
	ID                        string             `json:"id"`
	Status                    string             `json:"status"`
	Subject                   string             `json:"subject"`
	Question                  string             `json:"question,omitempty"`
	IncidentID                string             `json:"incidentId,omitempty"`
	IntegrationID             string             `json:"integrationId,omitempty"`
	WindowFrom                string             `json:"windowFrom"`
	WindowUntil               string             `json:"windowUntil"`
	ConclusionStatus          ConclusionStatus   `json:"conclusionStatus,omitempty"`
	Summary                   string             `json:"summary,omitempty"`
	Impact                    ImpactAssessment   `json:"impact"`
	Findings                  []findingView      `json:"findings"`
	Hypotheses                []HypothesisResult `json:"hypotheses"`
	Actions                   []ActionProposal   `json:"actions"`
	Limitations               []Limitation       `json:"limitations"`
	HumanConfirmationRequired bool               `json:"humanConfirmationRequired"`
	// StoppedBy labels a conclusion a ceiling forced — "spend", "tool_runs",
	// "reasoner_turns", "wall_clock", "stagnation", "context" — so a stopped
	// investigation never renders as a free diagnosis. Absent when the model concluded
	// freely.
	StoppedBy   string    `json:"stoppedBy,omitempty"`
	Error       string    `json:"error,omitempty"`
	Spend       spendView `json:"spend"`
	CreatedBy   string    `json:"createdBy,omitempty"`
	CreatedAt   string    `json:"createdAt"`
	ConcludedAt string    `json:"concludedAt,omitempty"`
}

type sourceView struct {
	IntegrationID string `json:"integrationId"`
	Rank          int    `json:"rank"`
	Reason        string `json:"reason"`
	SelectedAt    string `json:"selectedAt"`
}

type runView struct {
	Ordinal       int            `json:"ordinal"`
	IntegrationID string         `json:"integrationId,omitempty"`
	Tool          string         `json:"tool"`
	Purpose       string         `json:"purpose,omitempty"`
	HypothesisID  string         `json:"hypothesisId,omitempty"`
	Arguments     map[string]any `json:"arguments,omitempty"`
	WindowFrom    string         `json:"windowFrom"`
	WindowUntil   string         `json:"windowUntil"`
	Outcome       string         `json:"outcome"`
	Truncated     bool           `json:"truncated,omitempty"`
	Summary       string         `json:"summary,omitempty"`
	Sources       []string       `json:"sources,omitempty"`
	Error         string         `json:"error,omitempty"`
	StartedAt     string         `json:"startedAt"`
	FinishedAt    string         `json:"finishedAt"`
}

// detailView is one investigation with its whole provenance beside it: what was
// selected, what ran, what came of each.
type detailView struct {
	investigationView
	Sources []sourceView `json:"sources"`
	Runs    []runView    `json:"runs"`
}

type activityView struct {
	Sequence int64          `json:"sequence"`
	At       string         `json:"at"`
	Type     string         `json:"type"`
	Payload  map[string]any `json:"payload,omitempty"`
}

func investigationViewOf(found Investigation) investigationView {
	findings := make([]findingView, 0, len(found.Conclusion.Findings))
	for _, finding := range found.Conclusion.Findings {
		findings = append(findings, findingView{
			ID: finding.ID, Statement: finding.Statement, Kind: finding.Kind,
			Confidence: finding.Confidence, Mechanism: finding.Mechanism, RunRefs: finding.Sources,
		})
	}
	humanConfirmationRequired := false
	for _, action := range found.Conclusion.Actions {
		humanConfirmationRequired = humanConfirmationRequired || action.RequiresApproval
	}
	for _, limitation := range found.Conclusion.Limitations {
		humanConfirmationRequired = humanConfirmationRequired || limitation.Type == LimitationEssentialHumanInput
	}
	view := investigationView{
		ID:               found.ID.String(),
		Status:           publicStatus(found),
		Subject:          found.Subject,
		Question:         found.Question,
		WindowFrom:       stamp(found.WindowFrom),
		WindowUntil:      stamp(found.WindowUntil),
		ConclusionStatus: found.Conclusion.Status,
		Summary:          found.Conclusion.Summary, Impact: found.Conclusion.Impact,
		Findings: findings, Hypotheses: found.Conclusion.Hypotheses,
		Actions: found.Conclusion.Actions, Limitations: found.Conclusion.Limitations,
		HumanConfirmationRequired: humanConfirmationRequired,
		StoppedBy:                 found.StoppedBy,
		Error:                     found.Error,
		Spend: spendView{
			InputTokens:  found.Spend.InputTokens,
			OutputTokens: found.Spend.OutputTokens,
			MicroCents:   found.Spend.MicroCents,
		},
		CreatedBy: found.CreatedBy,
		CreatedAt: stamp(found.CreatedAt),
	}
	if found.IncidentID != uuid.Nil {
		view.IncidentID = found.IncidentID.String()
	}
	if found.IntegrationID != uuid.Nil {
		view.IntegrationID = found.IntegrationID.String()
	}
	if !found.ConcludedAt.IsZero() {
		view.ConcludedAt = stamp(found.ConcludedAt)
	}
	return view
}

func publicStatus(found Investigation) string {
	switch found.Status {
	case StatusRunning:
		if found.Executing {
			return "investigating"
		}
		return "queued"
	case StatusConcluded:
		for _, limitation := range found.Conclusion.Limitations {
			if limitation.Type == LimitationEssentialHumanInput {
				return "needs_input"
			}
		}
		if found.StoppedBy != "" || found.Conclusion.Status == Inconclusive {
			return "partial"
		}
		return "concluded"
	case StatusCancelled:
		return "cancelled"
	case StatusFailed:
		return "failed"
	default:
		return "unrecognised"
	}
}

func detailViewOf(found Investigation, sources []Source, runs []ToolRun) detailView {
	view := detailView{
		investigationView: investigationViewOf(found),
		Sources:           make([]sourceView, 0, len(sources)),
		Runs:              make([]runView, 0, len(runs)),
	}
	for _, source := range sources {
		view.Sources = append(view.Sources, sourceView{
			IntegrationID: source.IntegrationID.String(),
			Rank:          source.Rank,
			Reason:        source.Reason,
			SelectedAt:    stamp(source.SelectedAt),
		})
	}
	for _, run := range runs {
		rendered := runView{
			Ordinal:      run.Ordinal,
			Tool:         run.Tool,
			Purpose:      run.Purpose,
			HypothesisID: run.HypothesisID,
			Arguments:    run.Arguments,
			WindowFrom:   stamp(run.WindowFrom),
			WindowUntil:  stamp(run.WindowUntil),
			Outcome:      outcomeWord(run.Outcome),
			Truncated:    run.Truncated,
			Summary:      run.Summary,
			Sources:      run.Sources,
			Error:        run.Error,
			StartedAt:    stamp(run.StartedAt),
			FinishedAt:   stamp(run.FinishedAt),
		}
		if run.IntegrationID != uuid.Nil {
			rendered.IntegrationID = run.IntegrationID.String()
		}
		view.Runs = append(view.Runs, rendered)
	}
	return view
}

func outcomeWord(outcome RunOutcome) string {
	if outcome == RunSucceeded {
		return "succeeded"
	}
	return "failed"
}

func stamp(at time.Time) string { return at.UTC().Format(time.RFC3339) }

// writeJSON answers with a body. Nothing this surface returns may be cached: every answer
// concerns a named tenant's record.
func writeJSON(writer http.ResponseWriter, code int, body any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(code)
	_ = json.NewEncoder(writer).Encode(body)
}
