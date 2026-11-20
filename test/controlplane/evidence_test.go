package controlplane

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	storage "github.com/open-cluster/oc-control-plane/internal/store/postgres"
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
	var runIDs []uuid.UUID
	for _, ref := range refs {
		status, body := plane.call(t, http.MethodGet, plane.base(surfaceOrg)+"/investigations/"+ref.InvestigationID.String(), nil)
		var source struct {
			Runs []struct {
				ID                    uuid.UUID
				Ordinal               int
				Summary               string
				Arguments             map[string]any
				StartedAt, FinishedAt string
				DurationMs            float64
			}
		}
		decodeInto(t, body, &source)
		if status != http.StatusOK || len(source.Runs) != 1 || source.Runs[0].ID == uuid.Nil || source.Runs[0].Ordinal != 1 ||
			source.Runs[0].Summary != ref.InvestigationID.String() ||
			source.Runs[0].Arguments["namespace"] != "shop" {
			t.Fatalf("citation resolved to the wrong run: %d %s", status, body)
		}
		runIDs = append(runIDs, source.Runs[0].ID)
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
			ID              uuid.UUID
			ObservationText *string
			WindowApplied   *bool
		}
		decodeInto(t, retained, &observation)
		if code != http.StatusOK || observation.ID != source.Runs[0].ID ||
			observation.ObservationText != nil || observation.WindowApplied != nil {
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
	if runIDs[0] == runIDs[1] {
		t.Fatal("two Investigations' first runs shared an evidence identity")
	}
	refs[0] = investigation.EvidenceRef{ToolRunID: runIDs[0]}
	encodedRefs, err := json.Marshal(refs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(ctx, `UPDATE investigation
		SET conclusion = jsonb_set(conclusion, '{findings,0,evidenceRefs}', $3::jsonb)
		WHERE org_id = $1 AND investigation_id = $2`, surfaceOrg, ids[2], encodedRefs); err != nil {
		t.Fatal(err)
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
	privateChat := uuid.New()
	if _, err := database.Exec(ctx, `INSERT INTO conversation (org_id, conversation_id, source, subject)
		VALUES ($1, $2, 'slack', 'private conversation')`, surfaceOrg, privateChat); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(ctx, `UPDATE investigation SET conversation_id = $3, turn = 1
		WHERE org_id = $1 AND investigation_id = $2`, surfaceOrg, ids[1], privateChat); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "/runs/1/observation"} {
		path := plane.base(surfaceOrg) + "/investigations/" + ids[1].String() + suffix
		if status, body := plane.call(t, http.MethodGet, path, nil); status != http.StatusNotFound {
			t.Fatalf("a web read found private Slack evidence: %d %s", status, body)
		}
	}
	derived := plane.base(surfaceOrg) + "/investigations/" + ids[2].String()
	if status, body := plane.call(t, http.MethodGet, derived, nil); status != http.StatusNotFound {
		t.Fatalf("a retained web report exposed a restricted Slack citation: %d %s", status, body)
	}
	refs[1] = investigation.EvidenceRef{ToolRunID: runIDs[1]}
	encodedRefs, err = json.Marshal(refs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(ctx, `UPDATE investigation
		SET conclusion = jsonb_set(conclusion, '{findings,0,evidenceRefs}', $3::jsonb)
		WHERE org_id = $1 AND investigation_id = $2`, surfaceOrg, ids[2], encodedRefs); err != nil {
		t.Fatal(err)
	}
	if status, body := plane.call(t, http.MethodGet, derived, nil); status != http.StatusNotFound {
		t.Fatalf("a UUID-only Slack citation exposed its web report: %d %s", status, body)
	}
	webChat, current := uuid.New(), uuid.New()
	if _, err := database.Exec(ctx, `INSERT INTO conversation (org_id, conversation_id, source, subject)
		VALUES ($1, $2, 'web', 'web conversation')`, surfaceOrg, webChat); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(ctx, `UPDATE investigation SET conversation_id = $3, turn = 1
		WHERE org_id = $1 AND investigation_id = $2`, surfaceOrg, ids[2], webChat); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(ctx, `INSERT INTO investigation
		(investigation_id, org_id, subject, window_from, window_until, conversation_id, turn)
		VALUES ($1, $2, 'later turn', now()-interval '1 hour', now(), $3, 2)`, current, surfaceOrg, webChat); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(ctx, `INSERT INTO conversation_message
		(org_id, conversation_id, sequence, role, text, investigation_id, window_from, window_until)
		VALUES ($1, $2, 1, 2, 'prior restricted summary', $3, now()-interval '1 hour', now())`,
		surfaceOrg, webChat, ids[2]); err != nil {
		t.Fatal(err)
	}
	store, err := storage.OpenDatabase(ctx, plane.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	brief, err := store.ConversationBrief(ctx, uuid.MustParse(surfaceOrg), webChat, current, 1)
	if err != nil || len(brief.Findings) != 0 || len(brief.Recent) != 0 {
		t.Fatalf("a later web turn received restricted history: %+v, %v", brief, err)
	}
	if status, body := plane.call(t, http.MethodGet, plane.base(surfaceOrg)+"/conversations/"+webChat.String(), nil); status != http.StatusOK ||
		strings.Contains(body, "prior restricted summary") {
		t.Fatalf("the web Conversation exposed restricted history: %d %s", status, body)
	}

	if _, err := database.Exec(ctx, `
		INSERT INTO investigation_event (org_id, investigation_id, sequence, type, payload)
		VALUES ($1, $2, 1, $3, '{"text":"visible progress"}')`,
		surfaceOrg, current, int16(investigation.EventProgress)); err != nil {
		t.Fatal(err)
	}
	stream := openEventStream(t, plane, current.String(), "")
	defer func() { _ = stream.Body.Close() }()
	reader := bufio.NewReader(stream.Body)
	if first, err := reader.ReadString('\n'); err != nil || first != "id: 1\n" {
		t.Fatalf("the open stream did not emit public progress: %q, %v", first, err)
	}
	if _, err := database.Exec(ctx, `
		WITH concluded AS (
		    UPDATE investigation SET status = 2, concluded_at = now(),
		        conclusion = jsonb_build_object('findings', jsonb_build_array(jsonb_build_object('evidenceRefs',
		            jsonb_build_array(jsonb_build_object('toolRunId', $3::uuid)))))
		    WHERE org_id = $1 AND investigation_id = $2 RETURNING org_id, investigation_id
		)
		INSERT INTO investigation_event (org_id, investigation_id, sequence, type, payload)
		SELECT org_id, investigation_id, 2, $4, '{"summary":"restricted terminal summary"}'
		FROM concluded`, surfaceOrg, current, runIDs[1], int16(investigation.EventConcluded)); err != nil {
		t.Fatal(err)
	}
	events, err := io.ReadAll(reader)
	if err != nil || strings.Contains(string(events), "restricted terminal summary") {
		t.Fatalf("an open web stream exposed a restricted terminal event: %q, %v", events, err)
	}
}
