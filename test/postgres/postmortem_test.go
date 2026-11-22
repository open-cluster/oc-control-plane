package storage_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/postmortem"
)

func TestPostmortemLifecycleIsIncidentOwnedAndTenantScoped(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	registration := enrolledRelay(t, database, organization)
	integration := kubernetesIntegration(t, database, organization, registration)
	incident := recordIncident(t, database, organization, integration, "postmortem-incident")

	if _, err := database.GenerationInput(context.Background(), organization,
		incident); !errors.Is(err, postmortem.ErrNotEligible) {
		t.Fatalf("open incident generation error = %v", err)
	}
	pool, err := poolForTest(database, organization)
	if err != nil {
		t.Fatal(err)
	}
	principal := ownerOf(t, organization)
	if _, err = pool.Exec(context.Background(), `INSERT INTO app_user(user_id, issuer, subject, email)
		VALUES ($1, 'test', $2, 'reviewer@example.test')`, principal.UserID(), principal.UserID().String()); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(context.Background(), `
		UPDATE incident
		   SET status = 2, resolved_at = now(), updated_at = now()
		 WHERE incident_id = $1 AND org_id = $2`, incident, organization); err != nil {
		t.Fatal(err)
	}

	input, err := database.GenerationInput(context.Background(), organization, incident)
	if err != nil || input.IncidentID != incident || input.Title == "" || input.ResolvedAt.IsZero() {
		t.Fatalf("generation input = %+v err=%v", input, err)
	}
	draft := postmortem.DraftFrom(input)
	created, err := database.CreateDraft(context.Background(), principal,
		organization, draft)
	if err != nil || created.Revision != 1 || created.Status != postmortem.StatusDraft ||
		created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() {
		t.Fatalf("created = %+v err=%v", created, err)
	}

	draft.Revision = 2
	draft.Resolution = "Rolled back the deployment."
	replaced, err := database.ReplaceDraft(context.Background(), principal,
		organization, draft)
	if err != nil || replaced.Revision != 2 || replaced.Resolution != draft.Resolution {
		t.Fatalf("replaced = %+v err=%v", replaced, err)
	}
	impact := "Checkout failed for a subset of users."
	actions := []postmortem.ActionItem{{
		Title: "Bound database pools", Owner: "Database team", Deadline: "2026-09-15",
		Verification: "Database utilization remains below the limit.",
	}}
	corrected, err := database.Correct(context.Background(), principal,
		organization, incident, postmortem.Corrections{Impact: &impact, ActionItems: &actions})
	if err != nil || corrected.Impact != impact || corrected.ActionItems[0].Owner != "Database team" {
		t.Fatalf("corrected = %+v err=%v", corrected, err)
	}
	ctx := t.Context()
	runID, reportID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `
		WITH thread AS (
		    INSERT INTO conversation (org_id, conversation_id, source, subject)
		    VALUES ($1, gen_random_uuid(), 'slack', 'private thread') RETURNING conversation_id
		), message AS (
		    INSERT INTO conversation_message (org_id, conversation_id, sequence, role, text, window_from, window_until)
		    SELECT $1, conversation_id, 1, 1, 'private question', now()-interval '1 hour', now() FROM thread
		    RETURNING conversation_id, sequence
		), execution AS (
		    INSERT INTO agent_execution (org_id, execution_id, conversation_id, message_sequence, status)
		    SELECT $1, gen_random_uuid(), conversation_id, sequence, 'completed' FROM message RETURNING execution_id
		)
		INSERT INTO investigation_tool_run (org_id, tool_run_id, execution_id, tool, window_from,
		    window_until, outcome, started_at, finished_at)
		SELECT $1, $2, execution_id, 'read', now()-interval '1 hour', now(), 1, now(), now() FROM execution`,
		organization, runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO investigation (org_id, investigation_id, incident_id, subject, window_from,
		    window_until, status, concluded_at, conclusion)
		VALUES ($1, $2, $3, 'derived report', now()-interval '1 hour', now(), 2, now(),
		    jsonb_build_object('findings', jsonb_build_array(jsonb_build_object('evidenceRefs',
		        jsonb_build_array(jsonb_build_object('toolRunId', $4::uuid))))))`,
		organization, reportID, incident, runID); err != nil {
		t.Fatal(err)
	}

	timeline := []postmortem.TimelineEntry{{
		Description: "Unrecorded read", InvestigationID: uuid.NewString(), RunRefs: []int{1},
	}}
	if _, err := database.Correct(ctx, principal, organization, incident, postmortem.Corrections{Timeline: &timeline}); !errors.Is(err, postmortem.ErrUnavailableEvidence) {
		t.Errorf("correction accepted a dangling timeline ordinal: %v", err)
	}
	restricted := []postmortem.ActionItem{{
		Title: "Review private source", InvestigationID: reportID.String(),
	}}
	if _, err := database.Correct(ctx, principal, organization, incident, postmortem.Corrections{ActionItems: &restricted}); !errors.Is(err, postmortem.ErrUnavailableEvidence) {
		t.Errorf("correction accepted a restricted Investigation: %v", err)
	}
	reviewed, err := database.Review(ctx, principal, organization, incident)
	if err != nil || reviewed.Status != postmortem.StatusReviewed || reviewed.ReviewedAt.IsZero() ||
		reviewed.ReviewedByUserID == nil || *reviewed.ReviewedByUserID != principal.UserID() ||
		reviewed.Revision != corrected.Revision {
		t.Fatalf("reviewed = %+v err=%v", reviewed, err)
	}
	other := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	if _, err := database.Postmortem(ctx, other, incident); !errors.Is(err, postmortem.ErrUnknown) {
		t.Fatalf("other tenant read error = %v", err)
	}
	if reviewed.UpdatedAt.Before(created.CreatedAt.Add(-time.Second)) {
		t.Errorf("review timestamp moved backwards: created=%s reviewed=%s",
			created.CreatedAt, reviewed.UpdatedAt)
	}

	if _, err := pool.Exec(ctx, `
		UPDATE postmortem SET document = jsonb_set(document, '{rootCauses}',
		    jsonb_build_array(jsonb_build_object('evidenceRefs',
		        jsonb_build_array(jsonb_build_object('toolRunId', $3::uuid)))))
		WHERE org_id = $1 AND incident_id = $2`, organization, incident, runID); err != nil {
		t.Fatal(err)
	}
	input, err = database.GenerationInput(ctx, organization, incident)
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range input.Results {
		if result.InvestigationID == reportID {
			t.Error("Postmortem generation could read a report citing a private Slack Chat Tool Run")
		}
	}
	if _, err := database.Postmortem(ctx, organization, incident); !errors.Is(err, postmortem.ErrUnknown) {
		t.Errorf("a retained Postmortem with private UUID evidence was readable: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE postmortem SET document = jsonb_build_object(
		    'executiveSummary', 'summary retained from the derived report',
		    'timeline', jsonb_build_array(jsonb_build_object('investigationId', $3::uuid)))
		WHERE org_id = $1 AND incident_id = $2`, organization, incident, reportID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Postmortem(ctx, organization, incident); !errors.Is(err, postmortem.ErrUnknown) {
		t.Errorf("a Postmortem with a private derived summary was readable: %v", err)
	}
}
