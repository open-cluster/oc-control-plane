package investigation

import (
	"time"

	"github.com/google/uuid"
)

type findingView struct {
	Statement    string        `json:"statement"`
	Kind         FindingKind   `json:"kind,omitempty"`
	Mechanism    string        `json:"mechanism,omitempty"`
	RunRefs      []int         `json:"runRefs"`
	EvidenceRefs []EvidenceRef `json:"evidenceRefs,omitempty"`
}

type usageView struct {
	InputTokens  int64 `json:"inputTokens"`
	OutputTokens int64 `json:"outputTokens"`
}

type investigationView struct {
	ID               string             `json:"id"`
	Status           string             `json:"status"`
	Subject          string             `json:"subject"`
	IncidentID       string             `json:"incidentId,omitempty"`
	WindowFrom       string             `json:"windowFrom"`
	WindowUntil      string             `json:"windowUntil"`
	ConclusionStatus ConclusionStatus   `json:"conclusionStatus,omitempty"`
	Summary          string             `json:"summary,omitempty"`
	Impact           Impact             `json:"impact"`
	Findings         []findingView      `json:"findings"`
	Hypotheses       []HypothesisResult `json:"hypotheses"`
	Actions          []ActionProposal   `json:"actions"`
	Limitations      []Limitation       `json:"limitations"`
	StoppedBy        string             `json:"stoppedBy,omitempty"`
	Error            string             `json:"error,omitempty"`
	Usage            usageView          `json:"usage"`
	CreatedAt        string             `json:"createdAt"`
	ConcludedAt      string             `json:"concludedAt,omitempty"`
}

type runView struct {
	Ordinal       int            `json:"ordinal"`
	IntegrationID string         `json:"integrationId,omitempty"`
	Tool          string         `json:"tool"`
	Purpose       string         `json:"purpose,omitempty"`
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
	DurationMs    float64        `json:"durationMs"`
	WindowApplied *bool          `json:"windowApplied"`
	ModelClipped  bool           `json:"modelClipped"`
}

type observationView struct {
	runView
	InvestigationID string  `json:"investigationId"`
	ObservationText *string `json:"observationText"`
	IntegrationName string  `json:"integrationName,omitempty"`
	IntegrationType string  `json:"integrationType,omitempty"`
}

type detailView struct {
	investigationView
	Runs []runView `json:"runs"`
}

func investigationViewOf(found Investigation) investigationView {
	findings := make([]findingView, 0, len(found.Conclusion.Findings))
	for _, finding := range found.Conclusion.Findings {
		findings = append(findings, findingView(finding))
	}
	view := investigationView{
		ID:               found.ID.String(),
		Status:           found.Status.PublicStatus(found.Claimed),
		Subject:          found.Subject,
		WindowFrom:       found.WindowFrom.UTC().Format(time.RFC3339Nano),
		WindowUntil:      found.WindowUntil.UTC().Format(time.RFC3339Nano),
		ConclusionStatus: found.Conclusion.Status,
		Summary:          found.Conclusion.Summary,
		Impact:           found.Conclusion.Impact,
		Findings:         findings,
		Hypotheses:       found.Conclusion.Hypotheses,
		Actions:          found.Conclusion.Actions,
		Limitations:      found.Conclusion.Limitations,
		StoppedBy:        found.StoppedBy,
		Error:            found.Error,
		Usage: usageView{
			InputTokens:  found.Usage.InputTokens,
			OutputTokens: found.Usage.OutputTokens,
		},
		CreatedAt: found.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	if found.IncidentID != uuid.Nil {
		view.IncidentID = found.IncidentID.String()
	}
	if !found.ConcludedAt.IsZero() {
		view.ConcludedAt = found.ConcludedAt.UTC().Format(time.RFC3339Nano)
	}
	return view
}

func detailViewOf(found Investigation, runs []ToolRun) detailView {
	view := detailView{
		investigationView: investigationViewOf(found),
		Runs:              make([]runView, 0, len(runs)),
	}
	for _, run := range runs {
		view.Runs = append(view.Runs, runViewOf(run))
	}
	return view
}

func runViewOf(run ToolRun) runView {
	rendered := runView{
		Ordinal:       run.Ordinal,
		Tool:          run.Tool,
		Purpose:       run.Purpose,
		Arguments:     run.Arguments,
		WindowFrom:    run.WindowFrom.UTC().Format(time.RFC3339Nano),
		WindowUntil:   run.WindowUntil.UTC().Format(time.RFC3339Nano),
		Outcome:       outcomeWord(run.Outcome),
		Truncated:     run.Truncated,
		Summary:       run.Summary,
		Sources:       run.Sources,
		Error:         run.Error,
		StartedAt:     run.StartedAt.UTC().Format(time.RFC3339Nano),
		FinishedAt:    run.FinishedAt.UTC().Format(time.RFC3339Nano),
		DurationMs:    float64(run.FinishedAt.Sub(run.StartedAt)) / float64(time.Millisecond),
		WindowApplied: run.WindowApplied,
		ModelClipped:  run.ModelClipped,
	}
	if run.IntegrationID != uuid.Nil {
		rendered.IntegrationID = run.IntegrationID.String()
	}
	return rendered
}

func outcomeWord(outcome RunOutcome) string {
	if outcome == RunSucceeded {
		return "succeeded"
	}
	return "failed"
}
