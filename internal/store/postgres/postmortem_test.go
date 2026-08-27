package storage_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
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
	pool, err := database.Pool(organization)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(context.Background(), `
		UPDATE incident
		   SET status = 2, resolved_at = now(), updated_at = now()
		 WHERE incident_id = $1 AND org_id = $2`, incident, organization.String()); err != nil {
		t.Fatal(err)
	}

	input, err := database.GenerationInput(context.Background(), organization, incident)
	if err != nil || input.IncidentID != incident || input.Title == "" || input.ResolvedAt.IsZero() {
		t.Fatalf("generation input = %+v err=%v", input, err)
	}
	draft := postmortem.DraftFrom(input)
	created, err := database.CreateDraft(context.Background(), ownerOf(t, organization),
		organization, draft)
	if err != nil || created.Revision != 1 || created.Status != postmortem.StatusDraft ||
		created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() {
		t.Fatalf("created = %+v err=%v", created, err)
	}

	draft.Revision = 2
	draft.Resolution = "Rolled back the deployment."
	replaced, err := database.ReplaceDraft(context.Background(), ownerOf(t, organization),
		organization, draft)
	if err != nil || replaced.Revision != 2 || replaced.Resolution != draft.Resolution {
		t.Fatalf("replaced = %+v err=%v", replaced, err)
	}
	impact := "Checkout failed for a subset of users."
	actions := []postmortem.ActionItem{{
		Title: "Bound database pools", Owner: "Database team", Deadline: "2026-09-15",
		Verification: "Database utilization remains below the limit.", RunRefs: []int{3},
	}}
	corrected, err := database.Correct(context.Background(), ownerOf(t, organization),
		organization, incident, postmortem.Corrections{Impact: &impact, ActionItems: &actions})
	if err != nil || corrected.Impact != impact || corrected.ActionItems[0].Owner != "Database team" {
		t.Fatalf("corrected = %+v err=%v", corrected, err)
	}
	reviewed, err := database.Review(context.Background(), ownerOf(t, organization),
		organization, incident)
	if err != nil || reviewed.Status != postmortem.StatusReviewed || reviewed.ReviewedAt.IsZero() {
		t.Fatalf("reviewed = %+v err=%v", reviewed, err)
	}

	other, _ := tenancy.NewOrganization("other-org")
	if _, err := database.Postmortem(context.Background(), other, incident); !errors.Is(err, postmortem.ErrUnknown) {
		t.Fatalf("other tenant read error = %v", err)
	}
	if reviewed.UpdatedAt.Before(created.CreatedAt.Add(-time.Second)) {
		t.Errorf("review timestamp moved backwards: created=%s reviewed=%s",
			created.CreatedAt, reviewed.UpdatedAt)
	}
}
