package postmortem

import (
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

func DraftFrom(input GenerationInput) Postmortem {
	legacyRuns := make(map[historicalToolRun]uuid.UUID)
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
		if run.Ordinal > 0 && run.ID != uuid.Nil {
			legacyRuns[historicalToolRun{investigation: evidence.InvestigationID, ordinal: run.Ordinal}] = run.ID
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
	missingCausalEvidence := false
	for _, result := range input.Results {
		conclusion := result.Conclusion
		if conclusion.Summary != "" {
			draft.ExecutiveSummary = conclusion.Summary
		}
		if draft.Impact == NeedsHumanInput && len(reportEvidence(legacyRuns, result.InvestigationID, conclusion.Impact.RunRefs, conclusion.Impact.EvidenceRefs)) > 0 {
			draft.Impact = valueOr(conclusion.Impact.Summary, NeedsHumanInput)
		}
		for _, finding := range conclusion.Findings {
			if finding.Kind != investigation.FindingCause && finding.Kind != investigation.FindingContributingFactor {
				continue
			}
			cited := CitedStatement{InvestigationID: result.InvestigationID.String(),
				Statement:    finding.Statement,
				EvidenceRefs: reportEvidence(legacyRuns, result.InvestigationID, finding.RunRefs, finding.EvidenceRefs)}
			if len(cited.EvidenceRefs) == 0 {
				missingCausalEvidence = true
				continue
			}
			switch finding.Kind {
			case investigation.FindingCause:
				draft.RootCauses = append(draft.RootCauses, cited)
			case investigation.FindingContributingFactor:
				draft.ContributingFactors = append(draft.ContributingFactors, cited)
			}
		}
		for _, action := range conclusion.Actions {
			draft.ActionItems = append(draft.ActionItems, ActionItem{
				Title:           action.Title,
				Owner:           NeedsHumanInput,
				Deadline:        NeedsHumanInput,
				Verification:    action.Verification,
				InvestigationID: result.InvestigationID.String(),
				EvidenceRefs:    reportEvidence(legacyRuns, result.InvestigationID, action.RunRefs, action.EvidenceRefs),
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

// Historical reports may carry an ordinal in sections migration 0013 did not convert.
type historicalToolRun struct {
	investigation uuid.UUID
	ordinal       int
}

func reportEvidence(legacyRuns map[historicalToolRun]uuid.UUID, owner uuid.UUID, ordinals []int, refs []investigation.EvidenceRef) []investigation.EvidenceRef {
	var evidence []investigation.EvidenceRef
	add := func(id uuid.UUID) {
		ref := investigation.EvidenceRef{ToolRunID: id}
		if id != uuid.Nil && !slices.Contains(evidence, ref) {
			evidence = append(evidence, ref)
		}
	}
	for _, ref := range refs {
		id := ref.ToolRunID
		if id == uuid.Nil {
			id = legacyRuns[historicalToolRun{investigation: ref.InvestigationID, ordinal: ref.ToolRunOrdinal}]
		}
		add(id)
	}
	for _, ordinal := range ordinals {
		add(legacyRuns[historicalToolRun{investigation: owner, ordinal: ordinal}])
	}
	return evidence
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
