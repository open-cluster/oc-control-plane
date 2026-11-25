package postmortem

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

func TestDraftGeneratorKeepsMissingHumanFactsExplicit(t *testing.T) {
	t.Parallel()

	incidentID := uuid.New()
	priorID := uuid.New()
	runID := uuid.New()
	input := GenerationInput{
		IncidentID:  incidentID,
		Title:       "Checkout unavailable",
		FirstSeenAt: time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC),
		ResolvedAt:  time.Date(2026, 8, 27, 9, 30, 0, 0, time.UTC),
		AlertEvents: []AlertEvent{{Title: "CheckoutUnavailable",
			At: time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC)}},
		Runs: []RunEvidence{{
			InvestigationID: priorID,
			Run: investigation.ToolRun{
				ID: runID, Summary: "Checkout failed", Outcome: investigation.RunSucceeded,
				FinishedAt: time.Date(2026, 8, 27, 9, 4, 0, 0, time.UTC),
			},
		}},
		InvestigationTimeline: []TimelineEntry{{
			InvestigationID: priorID.String(),
			At:              time.Date(2026, 8, 27, 9, 2, 0, 0, time.UTC),
			Description:     "Investigation started",
		}},
		Messages: []Message{{Author: "On-call", Text: "the deploy may have caused this",
			At: time.Date(2026, 8, 27, 9, 3, 0, 0, time.UTC)}},
		Results: []InvestigationResult{{InvestigationID: priorID, Conclusion: investigation.Conclusion{
			Status:  investigation.VerifiedCause,
			Summary: "A bad pool-size deployment exhausted database connections.",
			Impact:  investigation.Impact{Summary: "Impact is not established."},
			Findings: []investigation.Finding{{
				Statement:    "The pool-size deployment exhausted connections.",
				Kind:         investigation.FindingCause,
				Mechanism:    "The larger pools exceeded the database connection limit.",
				EvidenceRefs: []investigation.EvidenceRef{{ToolRunID: runID}},
			}},
			Actions: []investigation.ActionProposal{{
				Title:        "Restore a bounded pool size",
				Verification: "Database utilization remains below the limit.",
				EvidenceRefs: []investigation.EvidenceRef{{ToolRunID: runID}},
			}},
			Hypotheses: []investigation.HypothesisResult{{
				Statement: "A traffic spike may also have contributed.",
				Status:    investigation.HypothesisUnresolved, Test: "Compare request volume with baseline.",
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
	if len(draft.RootCauses) != 1 || len(draft.RootCauses[0].EvidenceRefs) != 1 ||
		draft.RootCauses[0].EvidenceRefs[0] != (investigation.EvidenceRef{ToolRunID: runID}) {
		t.Errorf("the successful Tool Run UUID was not retained")
		t.Errorf("root causes = %+v", draft.RootCauses)
	}
	pruned := input
	pruned.Runs = nil
	prunedDraft := DraftFrom(pruned)
	if len(prunedDraft.RootCauses) != 1 || prunedDraft.RootCauses[0].Statement != NeedsHumanInput {
		t.Errorf("a pruned historical citation established a root cause: %+v", prunedDraft.RootCauses)
	}
	failed := input
	failed.Runs = append([]RunEvidence(nil), input.Runs...)
	failed.Runs[0].Run.Outcome = investigation.RunFailed
	if draft := DraftFrom(failed); draft.RootCauses[0].Statement != NeedsHumanInput || draft.ExecutiveSummary != NeedsHumanInput {
		t.Fatalf("a failed run established a cause: %+v", draft)
	}
	nativeRef := investigation.EvidenceRef{ToolRunID: runID}
	input.Results[0].Conclusion.Impact = investigation.Impact{
		Summary: "Checkout requests failed for 12 minutes.", EvidenceRefs: []investigation.EvidenceRef{nativeRef},
	}
	input.Results[0].Conclusion.Actions[0].EvidenceRefs = []investigation.EvidenceRef{nativeRef}
	native := DraftFrom(input)
	if native.Impact != "Checkout requests failed for 12 minutes." || native.Resolution != NeedsHumanInput ||
		native.ActionItems[0].Owner != NeedsHumanInput || native.ActionItems[0].Deadline != NeedsHumanInput ||
		len(native.ActionItems[0].EvidenceRefs) != 1 || native.ActionItems[0].EvidenceRefs[0] != nativeRef {
		t.Fatalf("native citations or missing facts were lost: %+v", native)
	}

	encoded, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "runRefs") {
		t.Errorf("new draft emitted ordinal citations: %s", encoded)
	}
	if strings.Contains(string(encoded), "findingId") {
		t.Errorf("Postmortem still exposes a Finding identity: %s", encoded)
	}
	if len(draft.ActionItems) != 1 || draft.ActionItems[0].Owner != NeedsHumanInput ||
		draft.ActionItems[0].Deadline != NeedsHumanInput {
		t.Errorf("action items = %+v", draft.ActionItems)
	}
	if len(draft.Timeline) < 3 ||
		!strings.Contains(strings.Join(draft.OpenQuestions, " "), "Verify testimony") ||
		!strings.Contains(strings.Join(draft.OpenQuestions, " "), "traffic spike") {
		t.Errorf("events/messages were not consumed safely: timeline=%+v questions=%+v",
			draft.Timeline, draft.OpenQuestions)
	}

	foundRun := false
	for _, entry := range draft.Timeline {
		if entry.Description == "Checkout failed" {
			foundRun = len(entry.EvidenceRefs) == 1 && entry.EvidenceRefs[0].ToolRunID == runID
		}
	}
	if !foundRun {
		t.Errorf("timeline omitted the retained Tool Run identity: %+v", draft.Timeline)
	}

	input.Results[0].Conclusion.Impact = investigation.Impact{
		Summary: "Checkout requests failed for 12 minutes.", EvidenceRefs: []investigation.EvidenceRef{nativeRef},
	}
	if cited := DraftFrom(input); cited.Impact != "Checkout requests failed for 12 minutes." {
		t.Errorf("cited impact = %q", cited.Impact)
	}
	input.Human.Impact = "Operators confirmed that premium checkout was affected."
	if corrected := DraftFrom(input); corrected.Impact != input.Human.Impact {
		t.Errorf("operator impact lost precedence: %q", corrected.Impact)
	}

	first := input.Results[0].Conclusion.Summary
	other := InvestigationResult{InvestigationID: uuid.New(), Conclusion: investigation.Conclusion{
		Status:  investigation.Inconclusive,
		Summary: first,
	}}
	input.Results = append(input.Results, other)
	if draft := DraftFrom(input); draft.ExecutiveSummary != first {
		t.Fatalf("repeated summary changed the executive summary: %q", draft.ExecutiveSummary)
	}
	input.Results[1].Conclusion.Summary = "The cause remains unresolved."
	conflicting := DraftFrom(input)
	if conflicting.ExecutiveSummary != NeedsHumanInput ||
		!strings.Contains(strings.Join(conflicting.OpenQuestions, " "), "Reconcile the differing Investigation summaries") {
		t.Fatalf("differing summaries silently produced %q with questions %v", conflicting.ExecutiveSummary, conflicting.OpenQuestions)
	}
	if len(conflicting.RootCauses) != 1 || len(conflicting.RootCauses[0].EvidenceRefs) != 1 ||
		conflicting.RootCauses[0].EvidenceRefs[0] != nativeRef {
		t.Fatalf("summary reconciliation lost the cited root cause: %+v", conflicting.RootCauses)
	}
}
