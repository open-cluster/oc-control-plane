package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
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
			conclusion.Findings = []investigation.Finding{{Statement: "Two prior observations.", Kind: investigation.FindingObservation,
				RunRefs: []int{}, EvidenceRefs: refs}}
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
				(investigation_id,org_id,ordinal,tool,arguments,window_from,window_until,outcome,summary,started_at,finished_at)
				VALUES ($1,$2,1,'read','{"namespace":"shop"}'::jsonb,now()-interval '1 hour',now(),1,$3,
				'2026-10-28T10:00:00.123456Z','2026-10-28T10:00:00.123789Z')`, id, surfaceOrg, id.String()); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, ref := range refs {
		status, body := plane.call(t, http.MethodGet, plane.base(surfaceOrg)+"/investigations/"+ref.InvestigationID.String(), nil)
		var source struct {
			Runs []struct {
				Ordinal               int
				Summary               string
				Arguments             map[string]any
				StartedAt, FinishedAt string
				DurationMs            float64
			}
		}
		decodeInto(t, body, &source)
		if status != http.StatusOK || len(source.Runs) != 1 || source.Runs[0].Ordinal != 1 ||
			source.Runs[0].Summary != ref.InvestigationID.String() ||
			source.Runs[0].Arguments["namespace"] != "shop" {
			t.Fatalf("citation resolved to the wrong run: %d %s", status, body)
		}
		if source.Runs[0].StartedAt != "2026-10-28T10:00:00.123456Z" ||
			source.Runs[0].FinishedAt != "2026-10-28T10:00:00.123789Z" || source.Runs[0].DurationMs != 0.333 {
			t.Fatalf("Tool execution precision or fractional duration was lost: %s", body)
		}
		var contract struct {
			Runs []map[string]json.RawMessage
		}
		decodeInto(t, body, &contract)
		if _, present := contract.Runs[0]["hypothesisId"]; present {
			t.Fatalf("Investigation detail still exposes hypothesisId: %s", body)
		}
		observationPath := plane.base(surfaceOrg) + "/investigations/" + ref.InvestigationID.String() + "/runs/1/observation"
		code, retained := plane.call(t, http.MethodGet, observationPath, nil)
		var observation struct {
			ObservationText *string
			WindowApplied   *bool
		}
		decodeInto(t, retained, &observation)
		if code != http.StatusOK || observation.ObservationText != nil || observation.WindowApplied != nil {
			t.Fatalf("legacy absence was represented as observed data or applied bounds: %d %s", code, retained)
		}
		if _, err := database.Exec(ctx, `UPDATE investigation_tool_run SET observation_text='' WHERE org_id=$1 AND investigation_id=$2 AND ordinal=1`, surfaceOrg, ref.InvestigationID); err != nil {
			t.Fatal(err)
		}
		code, retained = plane.call(t, http.MethodGet, observationPath, nil)
		decodeInto(t, retained, &observation)
		if code != http.StatusOK || observation.ObservationText == nil || *observation.ObservationText != "" {
			t.Fatal("a retained empty observation was confused with legacy absence")
		}
		maximum := strings.Repeat("界", 21845) + "a"
		if _, err := database.Exec(ctx, `UPDATE investigation_tool_run SET observation_text=$3 WHERE org_id=$1 AND investigation_id=$2 AND ordinal=1`, surfaceOrg, ref.InvestigationID, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := database.Exec(ctx, `UPDATE investigation_tool_run SET observation_text=$3 WHERE org_id=$1 AND investigation_id=$2 AND ordinal=1`, surfaceOrg, ref.InvestigationID, maximum+"界"); err == nil {
			t.Fatal("database accepted an observation beyond the UTF-8 byte ceiling")
		}
		code, retained = plane.call(t, http.MethodGet, observationPath, nil)
		decodeInto(t, retained, &observation)
		if code != http.StatusOK || observation.ObservationText == nil || *observation.ObservationText != maximum {
			t.Fatal("maximum bounded text did not round trip")
		}
		code, ordinary := plane.call(t, http.MethodGet, plane.base(surfaceOrg)+"/investigations/"+ref.InvestigationID.String(), nil)
		if code != http.StatusOK || len(ordinary) > 4096 || strings.Contains(ordinary, "observationText") {
			t.Fatal("retained text increased ordinary detail payload size")
		}
		restore := plane.switchOrganization(t, neighbourOrg)
		status, body = plane.call(t, http.MethodGet, plane.base(neighbourOrg)+"/investigations/"+ref.InvestigationID.String(), nil)
		restore()
		if status != http.StatusNotFound {
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
