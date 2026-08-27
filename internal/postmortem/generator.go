package postmortem

import (
	"strings"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

// DraftFrom creates a conservative first draft from durable evidence. It never turns
// testimony into system fact and leaves genuinely human fields explicit.
func DraftFrom(input GenerationInput) Postmortem {
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
			At: alert.At, Description: "Alert event: " + alert.Title, RunRefs: []int{},
		})
	}
	for _, evidence := range input.Runs {
		run := evidence.Run
		description := run.Purpose
		if description == "" {
			description = run.Summary
		}
		if description != "" {
			draft.Timeline = append(draft.Timeline, TimelineEntry{
				At: run.FinishedAt, Description: description,
				InvestigationID: evidence.InvestigationID.String(), RunRefs: []int{run.Ordinal},
			})
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
				InvestigationID: evidence.InvestigationID.String(), RunRefs: []int{},
			})
		}
	}
	if !input.ResolvedAt.IsZero() {
		draft.Timeline = append(draft.Timeline, TimelineEntry{
			At: input.ResolvedAt, Description: "Incident alerts resolved", RunRefs: []int{},
		})
	}
	for _, result := range input.Results {
		conclusion := result.Conclusion
		if conclusion.Summary != "" {
			draft.ExecutiveSummary = conclusion.Summary
		}
		if draft.Impact == NeedsHumanInput &&
			(conclusion.Impact.Status == investigation.ImpactKnown ||
				conclusion.Impact.Status == investigation.ImpactPartial) {
			draft.Impact = valueOr(conclusion.Impact.Summary, NeedsHumanInput)
		}
		for _, finding := range conclusion.Findings {
			cited := CitedStatement{InvestigationID: result.InvestigationID.String(),
				FindingID: finding.ID, Statement: finding.Statement,
				RunRefs: append([]int(nil), finding.Sources...)}
			switch finding.Kind {
			case investigation.FindingCause:
				draft.RootCauses = append(draft.RootCauses, cited)
			case investigation.FindingContributingFactor:
				draft.ContributingFactors = append(draft.ContributingFactors, cited)
			}
		}
		for _, action := range conclusion.Actions {
			draft.ActionItems = append(draft.ActionItems, ActionItem{
				Title: action.Title, Owner: NeedsHumanInput, Deadline: NeedsHumanInput,
				Verification:    action.Verification,
				InvestigationID: result.InvestigationID.String(),
				RunRefs:         append([]int(nil), action.RunRefs...),
			})
		}
		for _, limitation := range conclusion.Limitations {
			if limitation.Statement != "" {
				draft.OpenQuestions = append(draft.OpenQuestions, limitation.Statement)
			}
		}
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
		draft.Timeline = []TimelineEntry{{Description: NeedsHumanInput, RunRefs: []int{}}}
	}
	if len(draft.RootCauses) == 0 {
		draft.RootCauses = []CitedStatement{{Statement: NeedsHumanInput, RunRefs: []int{}}}
	}
	if len(draft.ContributingFactors) == 0 {
		draft.ContributingFactors = []CitedStatement{{Statement: NeedsHumanInput, RunRefs: []int{}}}
	}
	if len(draft.ActionItems) == 0 {
		draft.ActionItems = []ActionItem{{Title: NeedsHumanInput, Owner: NeedsHumanInput,
			Deadline: NeedsHumanInput, Verification: NeedsHumanInput, RunRefs: []int{}}}
	}
	if len(draft.OpenQuestions) == 0 {
		draft.OpenQuestions = []string{NeedsHumanInput}
	}
	return draft
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
