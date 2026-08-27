package postmortem

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

func TestDraftGeneratorKeepsMissingHumanFactsExplicit(t *testing.T) {
	t.Parallel()

	incidentID := uuid.New()
	input := GenerationInput{
		IncidentID:  incidentID,
		Title:       "Checkout unavailable",
		FirstSeenAt: time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC),
		ResolvedAt:  time.Date(2026, 8, 27, 9, 30, 0, 0, time.UTC),
		AlertEvents: []AlertEvent{{Title: "CheckoutUnavailable",
			At: time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC)}},
		Events: []EventEvidence{{InvestigationID: uuid.New(), Event: investigation.Event{
			At:   time.Date(2026, 8, 27, 9, 2, 0, 0, time.UTC),
			Type: investigation.EventStarted,
		}}},
		Messages: []Message{{Author: "On-call", Text: "the deploy may have caused this",
			At: time.Date(2026, 8, 27, 9, 3, 0, 0, time.UTC)}},
		Results: []InvestigationResult{{InvestigationID: uuid.New(), Conclusion: investigation.Conclusion{
			Status:  investigation.VerifiedCause,
			Summary: "A bad pool-size deployment exhausted database connections.",
			Impact: investigation.ImpactAssessment{
				Status: investigation.ImpactUnknown, CurrentState: "unknown",
				Summary: "Impact is not established.",
			},
			Findings: []investigation.Finding{{
				Statement: "The pool-size deployment exhausted connections.",
				Kind:      investigation.FindingCause, Confidence: investigation.ConfidenceConfirmed,
				Mechanism: "The larger pools exceeded the database connection limit.",
				Sources:   []int{3},
			}},
			Actions: []investigation.ActionProposal{{
				Title: "Restore a bounded pool size", Type: investigation.ActionFix,
				Verification: "Database utilization remains below the limit.", RunRefs: []int{3},
			}},
		}}},
	}

	draft := DraftFrom(input)
	if draft.IncidentID != incidentID || draft.Status != StatusDraft || draft.Revision != 1 {
		t.Fatalf("draft identity = %+v", draft)
	}
	if draft.Impact != NeedsHumanInput || draft.Resolution != NeedsHumanInput {
		t.Errorf("missing facts were invented: impact=%q resolution=%q",
			draft.Impact, draft.Resolution)
	}
	if len(draft.RootCauses) != 1 || len(draft.RootCauses[0].RunRefs) != 1 ||
		draft.RootCauses[0].RunRefs[0] != 3 {
		t.Errorf("root causes = %+v", draft.RootCauses)
	}
	if len(draft.ActionItems) != 1 || draft.ActionItems[0].Owner != NeedsHumanInput ||
		draft.ActionItems[0].Deadline != NeedsHumanInput {
		t.Errorf("action items = %+v", draft.ActionItems)
	}
	if len(draft.Timeline) < 3 ||
		!strings.Contains(strings.Join(draft.OpenQuestions, " "), "Verify testimony") {
		t.Errorf("events/messages were not consumed safely: timeline=%+v questions=%+v",
			draft.Timeline, draft.OpenQuestions)
	}
}
