package postmortem

import (
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

func DraftFrom(input GenerationInput) Postmortem {
	successful := make(map[uuid.UUID]bool)
	draft := Postmortem{
		IncidentID: input.IncidentID, Status: StatusDraft, Revision: 1,
		Title:            valueOr(input.Title, NeedsHumanInput),
		ExecutiveSummary: NeedsHumanInput,
		Impact:           valueOr(input.Human.Impact, NeedsHumanInput),
		Detection:        NeedsHumanInput,
		Resolution:       valueOr(input.Human.Resolution, NeedsHumanInput),
		WhatWentWell:     stringsOrHuman(input.Human.WhatWentWell),
		WhatWentWrong:    stringsOrHuman(input.Human.WhatWentWrong),
		DetectionGaps:    stringsOrHuman(input.Human.DetectionGaps),
		OpenQuestions:    []string{},
	}
	if len(input.AlertEvents) > 0 {
		draft.Detection = "Detected by alert event: " + input.AlertEvents[0].Title
	}
	for _, alert := range input.AlertEvents {
		draft.Timeline = append(draft.Timeline, TimelineEntry{
			At: alert.At, Description: "Alert event: " + alert.Title,
		})
	}
	for _, evidence := range input.Runs {
		run := evidence.Run
		description := run.Purpose
		if description == "" {
			description = run.Summary
		}
		if run.Outcome == investigation.RunSucceeded && run.ID != uuid.Nil {
			successful[run.ID] = true
		}
		if description != "" {
			entry := TimelineEntry{
				At: run.FinishedAt, Description: description,
				InvestigationID: evidence.InvestigationID.String(),
			}
			if run.ID != uuid.Nil {
				entry.EvidenceRefs = []investigation.EvidenceRef{{ToolRunID: run.ID}}
			}
			draft.Timeline = append(draft.Timeline, entry)
		}
	}
	for _, evidence := range input.Events {
		description := ""
		switch evidence.Event.Type {
		case investigation.EventStarted:
			description = "Investigation started"
		case investigation.EventConcluded:
			description = "Investigation concluded"
		case investigation.EventCancelled:
			description = "Investigation cancelled"
		case investigation.EventFailed:
			description = "Investigation failed"
		}
		if description != "" {
			draft.Timeline = append(draft.Timeline, TimelineEntry{
				At: evidence.Event.At, Description: description,
				InvestigationID: evidence.InvestigationID.String(),
			})
		}
	}
	if !input.ResolvedAt.IsZero() {
		draft.Timeline = append(draft.Timeline, TimelineEntry{
			At: input.ResolvedAt, Description: "Incident alerts resolved",
		})
	}
	var summary string
	var missingCausalEvidence, summariesDiffer bool
	for _, result := range input.Results {
		conclusion := result.Conclusion
		if conclusion.Status == "" || conclusion.Status == investigation.HistoricalAnswerOnly {
			continue
		}
		impact, incomplete := reportEvidence(successful, conclusion.Impact.EvidenceRefs)
		if draft.Impact == NeedsHumanInput && len(impact) > 0 && !incomplete {
			draft.Impact = valueOr(conclusion.Impact.Summary, NeedsHumanInput)
		}
		var missingCause, citedCause bool
		for _, finding := range conclusion.Findings {
			if finding.Kind != investigation.FindingCause && finding.Kind != investigation.FindingContributingFactor {
				continue
			}
			refs, incomplete := reportEvidence(successful, finding.EvidenceRefs)
			if len(refs) == 0 || incomplete {
				missingCause = true
				continue
			}
			citedCause = true
			cited := CitedStatement{
				InvestigationID: result.InvestigationID.String(),
				Statement:       finding.Statement,
				EvidenceRefs:    refs,
			}
			switch finding.Kind {
			case investigation.FindingCause:
				draft.RootCauses = append(draft.RootCauses, cited)
			case investigation.FindingContributingFactor:
				draft.ContributingFactors = append(draft.ContributingFactors, cited)
			}
		}
		if conclusion.Status != investigation.Inconclusive && !citedCause {
			missingCause = true
		}
		missingCausalEvidence = missingCausalEvidence || missingCause
		if candidate := strings.TrimSpace(conclusion.Summary); !missingCause && candidate != "" {
			if summary == "" {
				summary = conclusion.Summary
			}
			summariesDiffer = summariesDiffer || candidate != strings.TrimSpace(summary)
		}
		for _, action := range conclusion.Actions {
			refs, _ := reportEvidence(successful, action.EvidenceRefs)
			draft.ActionItems = append(draft.ActionItems, ActionItem{
				Title:           action.Title,
				Owner:           NeedsHumanInput,
				Deadline:        NeedsHumanInput,
				Verification:    action.Verification,
				InvestigationID: result.InvestigationID.String(),
				EvidenceRefs:    refs,
			})
		}
		for _, limitation := range conclusion.Limitations {
			if limitation.Statement != "" {
				draft.OpenQuestions = append(draft.OpenQuestions, limitation.Statement)
			}
		}
		for _, hypothesis := range conclusion.Hypotheses {
			if hypothesis.Status == investigation.HypothesisUnresolved && hypothesis.Statement != "" {
				draft.OpenQuestions = append(draft.OpenQuestions, hypothesis.Statement)
			}
		}
	}
	if missingCausalEvidence {
		draft.OpenQuestions = append(draft.OpenQuestions, investigation.MissingEvidenceStatement)
	}
	if summariesDiffer {
		draft.OpenQuestions = append(draft.OpenQuestions, "Reconcile the differing Investigation summaries before finalizing the executive summary.")
	} else if summary != "" {
		draft.ExecutiveSummary = summary
	}
	for _, message := range input.Messages {
		author := valueOr(message.Author, "operator")
		text := strings.TrimSpace(message.Text)
		if text == "" {
			continue
		}
		if len(text) > 512 {
			text = text[:512]
		}
		draft.OpenQuestions = append(draft.OpenQuestions,
			"Verify testimony from "+author+": "+text)
	}
	if len(draft.Timeline) == 0 {
		draft.Timeline = []TimelineEntry{{Description: NeedsHumanInput}}
	}
	if len(draft.RootCauses) == 0 {
		draft.RootCauses = []CitedStatement{{Statement: NeedsHumanInput}}
	}
	if len(draft.ContributingFactors) == 0 {
		draft.ContributingFactors = []CitedStatement{{Statement: NeedsHumanInput}}
	}
	if len(draft.ActionItems) == 0 {
		draft.ActionItems = []ActionItem{{Title: NeedsHumanInput, Owner: NeedsHumanInput,
			Deadline: NeedsHumanInput, Verification: NeedsHumanInput}}
	}
	if len(draft.OpenQuestions) == 0 {
		draft.OpenQuestions = []string{NeedsHumanInput}
	}
	return draft
}

func reportEvidence(successful map[uuid.UUID]bool, refs []investigation.EvidenceRef) ([]investigation.EvidenceRef, bool) {
	var evidence []investigation.EvidenceRef
	var incomplete bool
	for _, ref := range refs {
		if !successful[ref.ToolRunID] {
			incomplete = true
			continue
		}
		if !slices.Contains(evidence, ref) {
			evidence = append(evidence, ref)
		}
	}
	return evidence, incomplete
}

func valueOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func stringsOrHuman(values []string) []string {
	if len(values) == 0 {
		return []string{NeedsHumanInput}
	}
	return append([]string(nil), values...)
}
