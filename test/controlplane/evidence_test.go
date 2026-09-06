package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

func TestEvidenceReferencesResolveDistinctRunsAndSurvivePruning(t *testing.T) {
	plane := startIntegrationPlane(t)
	ctx := context.Background()
	database, err := pgx.Connect(ctx, plane.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close(ctx) }()
	ids := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	refs := []investigation.EvidenceRef{{InvestigationID: ids[0], ToolRunOrdinal: 1}, {InvestigationID: ids[1], ToolRunOrdinal: 1}}
	for index, id := range ids {
		conclusion := investigation.Conclusion{Status: investigation.AnswerOnly, Summary: "Prior observations."}
		if index == 2 {
			conclusion.Findings = []investigation.Finding{{ID: "reused", Statement: "Two prior observations.", Kind: investigation.FindingObservation,
				Confidence: investigation.ConfidenceConfirmed, Sources: []int{}, EvidenceRefs: refs}}
		}
		encoded, err := json.Marshal(conclusion)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = database.Exec(ctx, `INSERT INTO investigation
			(investigation_id, org_id, subject, window_from, window_until, status, concluded_at, conclusion)
			VALUES ($1,$2,'citation fixture',now()-interval '1 hour',now(),2,now(),$3)`, id, surfaceOrg, encoded); err != nil {
			t.Fatal(err)
		}
		if index < 2 {
			if _, err = database.Exec(ctx, `INSERT INTO investigation_tool_run
				(investigation_id,org_id,ordinal,tool,window_from,window_until,outcome,summary,started_at,finished_at)
				VALUES ($1,$2,1,'read',now()-interval '1 hour',now(),1,$3,now(),now())`, id, surfaceOrg, id.String()); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, ref := range refs {
		status, body := plane.call(t, http.MethodGet, plane.base(surfaceOrg)+"/investigations/"+ref.InvestigationID.String(), nil)
		var source struct {
			Runs []struct {
				Ordinal int
				Summary string
			}
		}
		decodeInto(t, body, &source)
		if status != http.StatusOK || len(source.Runs) != 1 || source.Runs[0].Ordinal != 1 || source.Runs[0].Summary != ref.InvestigationID.String() {
			t.Fatalf("citation resolved to the wrong run: %d %s", status, body)
		}
		if status, body = plane.call(t, http.MethodGet, plane.base(neighbourOrg)+"/investigations/"+ref.InvestigationID.String(), nil); status != http.StatusNotFound {
			t.Fatalf("cross-Organization citation disclosed data: %d %s", status, body)
		}
	}
	for _, prune := range []bool{false, true} {
		if prune {
			if _, err = database.Exec(ctx, `DELETE FROM investigation_tool_run WHERE org_id=$1 AND investigation_id=$2`, surfaceOrg, ids[0]); err != nil {
				t.Fatal(err)
			}
		}
		status, body := plane.call(t, http.MethodGet, plane.base(surfaceOrg)+"/investigations/"+ids[2].String(), nil)
		var result struct {
			Findings    []investigation.Finding
			Limitations []investigation.Limitation
		}
		decodeInto(t, body, &result)
		if status != http.StatusOK || len(result.Findings) != 1 || !slices.Equal(result.Findings[0].EvidenceRefs, refs) {
			t.Fatalf("lost evidence origins: %d %s", status, body)
		}
		missing := slices.ContainsFunc(result.Limitations, func(item investigation.Limitation) bool { return item.Type == investigation.LimitationMissingTelemetry })
		if missing != prune {
			t.Fatalf("wrong evidence availability: %s", body)
		}
		if prune && result.Limitations[0].RunRefs == nil {
			t.Fatalf("missing-evidence limitation has null runRefs: %s", body)
		}
	}
}
