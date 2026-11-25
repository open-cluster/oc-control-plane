package storage_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/conversation"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	storage "github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func TestAgentToolRunsSurviveLeaseTakeover(t *testing.T) {
	database, org := migratedDatabase(t)
	ctx := t.Context()
	chat := openConversation(t, database, org, "checkout")
	principal := executionOwner(t, database, org)
	admitted, err := database.AdmitAgentChat(ctx, principal, org, chat.ID, uuid.New(),
		conversation.NewMessage{Text: "Check checkout."})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := database.ClaimAgentExecution(ctx, org, admitted.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	begin := func() uuid.UUID {
		t.Helper()
		id, err := database.BeginAgentToolRun(ctx, org, claim, investigation.ToolRun{
			Tool: "query_metrics", WindowFrom: now.Add(-time.Hour), WindowUntil: now, StartedAt: now,
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	first, second, interrupted := begin(), begin(), begin()
	if first == uuid.Nil || second == uuid.Nil || first == second {
		t.Fatalf("attempts received invalid identities %s and %s", first, second)
	}
	observation := "a bounded observation"
	complete := func(execution storage.AgentExecution, id uuid.UUID) error {
		return database.CompleteAgentToolRun(ctx, org, execution, investigation.ToolRun{
			ID: id, Outcome: investigation.RunSucceeded, FinishedAt: now.Add(time.Second),
			ObservationText: &observation,
		}, nil)
	}
	for _, id := range []uuid.UUID{second, first} {
		if err := complete(claim, id); err != nil {
			t.Fatal(err)
		}
	}
	observation = "different observation"
	if err := complete(claim, first); err == nil {
		t.Fatal("committed Tool Run was accepted for completion again")
	}
	if err := database.SaveChatDraft(ctx, org, claim, "Provisional"); err != nil {
		t.Fatal(err)
	}
	assertActivity := func(draft, status string) {
		t.Helper()
		snapshot, err := database.ChatExecutionSnapshot(ctx, org, chat.ID, claim.ID)
		if err != nil || snapshot.DraftText != draft || len(snapshot.Tools) != 3 {
			t.Fatalf("unexpected execution snapshot: %+v, %v", snapshot, err)
		}
		for _, check := range []struct {
			id     uuid.UUID
			status string
		}{
			{first, "succeeded"}, {second, "succeeded"}, {interrupted, status},
		} {
			if !slices.ContainsFunc(snapshot.Tools, func(run conversation.ToolActivity) bool {
				return run.ToolRunID == check.id && run.Status == check.status
			}) {
				t.Fatalf("Tool Run %s did not appear as %s: %+v", check.id, check.status, snapshot.Tools)
			}
		}
	}
	assertActivity("Provisional", "running")
	pool, err := poolForTest(database, org)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_execution SET lease_expires_at = clock_timestamp()
		WHERE org_id = $1 AND execution_id = $2`, org, claim.ID); err != nil {
		t.Fatal(err)
	}
	assertActivity("", "failed")
	if err := database.SaveChatDraft(ctx, org, claim, "Stale draft"); !errors.Is(err, storage.ErrAgentLeaseLost) {
		t.Fatalf("expired draft write = %v", err)
	}
	recovered, err := database.ClaimAgentExecution(ctx, org, claim.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	assertActivity("", "failed")
	if _, err := pool.Exec(ctx, `UPDATE organization_membership SET role = 'viewer'
		WHERE org_id = $1 AND user_id = $2`, org, principal.UserID()); err != nil {
		t.Fatal(err)
	}
	if err := database.SaveChatDraft(ctx, org, recovered, "Unauthorized text"); !errors.Is(err, authz.ErrNotAMember) {
		t.Fatalf("draft write after author lost permission = %v", err)
	}
	if err := complete(claim, interrupted); !errors.Is(err, storage.ErrAgentLeaseLost) {
		t.Fatalf("stale worker completed an interrupted Tool Run: %v", err)
	}
	if err := complete(recovered, interrupted); err == nil {
		t.Fatal("new worker published an observation under an old attempt")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM agent_execution
		WHERE org_id = $1 AND execution_id = $2`, org, claim.ID); err == nil {
		t.Fatal("deleting an execution discarded its Tool Runs")
	}
	for _, check := range []struct {
		id      uuid.UUID
		missing bool
	}{
		{id: first}, {id: second}, {id: interrupted, missing: true}, {id: uuid.New(), missing: true},
	} {
		missing, err := database.EvidenceMissing(ctx, org, uuid.Nil, []investigation.EvidenceRef{{ToolRunID: check.id}})
		if err != nil || missing != check.missing {
			t.Fatalf("evidence %s missing = %t, want %t, error = %v", check.id, missing, check.missing, err)
		}
	}
	for _, id := range []uuid.UUID{first, second} {
		var retained string
		if err := pool.QueryRow(ctx, `SELECT observation_text FROM investigation_tool_run
			WHERE org_id = $1 AND execution_id = $2 AND tool_run_id = $3 AND investigation_id IS NULL`,
			org, claim.ID, id).Scan(&retained); err != nil || retained != "a bounded observation" {
			t.Fatalf("retained observation = %q, error = %v", retained, err)
		}
	}
}

func TestDeleteChatFencesExecutionAndRetainsToolRunIdentity(t *testing.T) {
	db, org := migratedDatabase(t)
	ctx := t.Context()
	principal := executionOwner(t, db, org)
	chat := openConversation(t, db, org, "Operational notes")
	first, err := db.AdmitAgentChat(ctx, principal, org, chat.ID, uuid.New(),
		conversation.NewMessage{Text: "Private question"})
	if err != nil {
		t.Fatal(err)
	}
	active, err := db.ClaimAgentExecution(ctx, org, first.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := db.AdmitAgentChat(ctx, principal, org, chat.ID, uuid.New(),
		conversation.NewMessage{Text: "Still pending"})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := poolForTest(db, org)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_execution SET status = 'needs_input'
		WHERE org_id = $1 AND execution_id = $2`, org, waiting.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	run, err := db.BeginAgentToolRun(ctx, org, active, investigation.ToolRun{
		Tool: "query_metrics", WindowFrom: now.Add(-time.Hour), WindowUntil: now, StartedAt: now,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	saver := db.AgentStateSaver(org, active)
	state := &agent.AgentState{SchemaVersion: 1, SessionID: active.SessionID(), Summary: "private context"}
	if err := saver.SaveState(ctx, active.SessionID(), state); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteConversation(ctx, principal, org, chat.ID); err != nil {
		t.Fatal(err)
	}
	if err := saver.SaveState(ctx, active.SessionID(), state); !errors.Is(err, storage.ErrAgentLeaseLost) {
		t.Fatalf("late checkpoint write = %v", err)
	}
	if err := db.FinishAgentExecution(ctx, org, active, storage.AgentExecutionCompleted,
		"late reply", nil, investigation.Usage{}); !errors.Is(err, storage.ErrAgentLeaseLost) {
		t.Fatalf("late reply = %v", err)
	}
	if err := db.CompleteAgentToolRun(ctx, org, active, investigation.ToolRun{
		ID: run, Outcome: investigation.RunSucceeded, FinishedAt: now.Add(time.Second),
	}, nil); !errors.Is(err, storage.ErrAgentLeaseLost) {
		t.Fatalf("late Tool Run = %v", err)
	}
	var messages, checkpoints, executions, runs, auditEvents int
	err = pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM conversation_message WHERE org_id = $1 AND conversation_id = $2 AND text <> ''),
		(SELECT count(*) FROM agent_checkpoint WHERE org_id = $1 AND conversation_id = $2),
		(SELECT count(*) FROM agent_execution WHERE org_id = $1 AND conversation_id = $2 AND status = 'cancelled'),
		(SELECT count(*) FROM investigation_tool_run WHERE org_id = $1 AND tool_run_id = $3 AND outcome = $4),
		(SELECT count(*) FROM audit_event WHERE org_id = $1 AND target_id = $2::text AND action = 'conversation.deleted')`,
		org, chat.ID, run, int16(investigation.RunFailed)).Scan(&messages, &checkpoints, &executions, &runs, &auditEvents)
	if err != nil || messages != 0 || checkpoints != 0 || executions != 2 || runs != 1 || auditEvents != 1 {
		t.Fatalf("after deletion Messages=%d checkpoints=%d cancellations=%d Tool Runs=%d audits=%d: %v",
			messages, checkpoints, executions, runs, auditEvents, err)
	}
}

func TestLinkedInvestigationRechecksItsRequester(t *testing.T) {
	db, org := migratedDatabase(t)
	ctx := t.Context()
	principal := executionOwner(t, db, org)
	chat := openConversation(t, db, org, "Checkout")
	now := time.Now()
	opened, err := db.CreateInvestigation(ctx, principal, org, investigation.NewInvestigation{
		ConversationID: chat.ID, Subject: "Investigate latency",
		WindowFrom: now.Add(-time.Hour), WindowUntil: now,
	}, 4)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := db.ClaimAgentExecution(ctx, org, opened.ExecutionID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, origin, err := db.AgentToolScope(ctx, org, claimed); err != nil || origin != nil {
		t.Fatalf("linked Investigation could not use requester's authority: %v, %v", origin, err)
	}
	pool, err := poolForTest(db, org)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE organization_membership SET role = 'viewer'
		WHERE org_id = $1 AND user_id = $2`, org, principal.UserID()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.AgentToolScope(ctx, org, claimed); !errors.Is(err, authz.ErrNotAMember) {
		t.Fatalf("revoked requester still has Tool access: %v", err)
	}
}

func TestAgentExecution(t *testing.T) {
	database, organization, foreignOrg := twoOrganizationsInOneDatabase(t)
	principal := executionOwner(t, database, organization)

	t.Run("new Chat turn restores the prior turn's checkpoint", func(t *testing.T) {
		ctx := t.Context()
		chat := openConversation(t, database, organization, "checkout")
		first, err := database.AdmitAgentChat(ctx, principal, organization, chat.ID, uuid.New(),
			conversation.NewMessage{Text: "Remember the checkout port."})
		if err != nil {
			t.Fatal(err)
		}
		pool, err := poolForTest(database, organization)
		if err != nil {
			t.Fatal(err)
		}
		// A checkpoint insert takes key share on its Conversation.
		fkCheck, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = fkCheck.Rollback(ctx) }()
		if _, err := fkCheck.Exec(ctx, `SELECT 1 FROM conversation
			WHERE org_id = $1 AND conversation_id = $2 FOR KEY SHARE`, organization, chat.ID); err != nil {
			t.Fatal(err)
		}
		claimCtx, cancelClaim := context.WithTimeout(ctx, 3*time.Second)
		defer cancelClaim()
		first, err = database.ClaimAgentExecution(claimCtx, organization, first.ID, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if err := fkCheck.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		saver := database.AgentStateSaver(organization, first)
		session := first.SessionID()
		snapshot := &agent.AgentState{SchemaVersion: 1, SessionID: session, Summary: "Checkout uses port 8443."}
		if err := saver.SaveState(ctx, session, snapshot); err != nil {
			t.Fatal(err)
		}
		if err := database.FinishAgentExecution(ctx, organization, first, storage.AgentExecutionCompleted, "Port saved.", nil, investigation.Usage{}); err != nil {
			t.Fatal(err)
		}

		if _, err := pool.Exec(ctx, "DELETE FROM agent_execution WHERE org_id = $1 AND execution_id = $2", organization, first.ID); err != nil {
			t.Fatal(err)
		}

		next, err := database.AdmitAgentChat(ctx, principal, organization, chat.ID, uuid.New(),
			conversation.NewMessage{Text: "Which port?"})
		if err != nil {
			t.Fatal(err)
		}
		next, err = database.ClaimAgentExecution(ctx, organization, next.ID, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		restored, err := agent.LoadCheckpoint(ctx, database.AgentStateSaver(organization, next), next.SessionID())
		if err != nil || restored.Summary != snapshot.Summary || restored.SessionID != session {
			t.Fatalf("restored checkpoint = %+v, error = %v", restored, err)
		}
		nextSaver := database.AgentStateSaver(organization, next)
		active := *snapshot
		active.Summary = "unfinished turn"
		if err := nextSaver.SaveState(ctx, session, &active); err != nil {
			t.Fatal(err)
		}
		if err := nextSaver.DeleteSession(ctx, session); err != nil {
			t.Fatal(err)
		}
		restored, err = nextSaver.LoadState(ctx, session)
		if err != nil || restored == nil || restored.Summary != snapshot.Summary {
			t.Fatalf("deleting active state lost committed context: %v", err)
		}
		if err := saver.DeleteSession(ctx, session); !errors.Is(err, storage.ErrAgentLeaseLost) {
			t.Fatalf("prior Chat turn deleted the checkpoint: %v", err)
		}
	})

	t.Run("Investigation checkpoint stays within the current lease and Organization", func(t *testing.T) {
		ctx := t.Context()
		created, err := database.CreateInvestigation(ctx, principal, organization, investigation.NewInvestigation{
			Subject: "checkout errors", WindowFrom: time.Now().Add(-time.Hour), WindowUntil: time.Now(),
		}, 8)
		if err != nil {
			t.Fatal(err)
		}
		queued := investigationExecutionID(t, database, organization, created.ID)
		if _, err := database.ClaimAgentExecution(ctx, foreignOrg, queued, time.Minute); !errors.Is(err, storage.ErrAgentExecutionUnavailable) {
			t.Fatalf("foreign execution claimed: %v", err)
		}
		claim, err := database.ClaimAgentExecution(ctx, organization, queued, time.Minute)
		if err != nil {
			t.Fatal(err)
		}

		active := database.AgentStateSaver(organization, claim)
		session := claim.SessionID()
		checkpoint := &agent.AgentState{SchemaVersion: 1, SessionID: session, Summary: "One service was unavailable."}

		if err := database.AgentStateSaver(foreignOrg, claim).SaveState(ctx, session, checkpoint); !errors.Is(err, storage.ErrAgentLeaseLost) {
			t.Fatalf("foreign checkpoint written: %v", err)
		}
		mismatched := *checkpoint
		mismatched.SessionID = "another-session"
		if err := active.SaveState(ctx, session, &mismatched); !errors.Is(err, storage.ErrAgentCheckpoint) {
			t.Fatalf("mismatched native session saved: %v", err)
		}
		if err := active.SaveState(ctx, session, checkpoint); err != nil {
			t.Fatal(err)
		}
		pool, err := poolForTest(database, organization)
		if err != nil {
			t.Fatal(err)
		}
		blocker, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = blocker.Rollback(ctx) }()
		if _, err := blocker.Exec(ctx, `SELECT state FROM agent_checkpoint
			WHERE org_id = $1 AND session_id = $2 FOR UPDATE`, organization, session); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE agent_execution
			SET lease_expires_at = clock_timestamp() + interval '4 seconds'
			WHERE org_id = $1 AND execution_id = $2`, organization, claim.ID); err != nil {
			t.Fatal(err)
		}
		late := *checkpoint
		late.Summary = "This arrived past the lease."
		attempt := make(chan error, 1)
		go func() { attempt <- active.SaveState(ctx, session, &late) }()
		deadline := time.Now().Add(3 * time.Second)
		for {
			var blocked bool
			if err := pool.QueryRow(ctx, `SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE datname = current_database() AND wait_event_type = 'Lock'
				  AND query LIKE '%INSERT INTO agent_checkpoint%')`).Scan(&blocked); err != nil {
				t.Fatal(err)
			}
			if blocked {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("checkpoint write did not wait on the held row")
			}
			time.Sleep(20 * time.Millisecond)
		}
		if _, err := pool.Exec(ctx, `SELECT pg_sleep(greatest(0,
			extract(epoch FROM lease_expires_at - clock_timestamp()) + 0.02))
			FROM agent_execution WHERE org_id = $1 AND execution_id = $2`, organization, claim.ID); err != nil {
			t.Fatal(err)
		}
		if err := blocker.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-attempt; !errors.Is(err, storage.ErrAgentLeaseLost) {
			t.Fatalf("checkpoint write committed after its lease expired: %v", err)
		}
		if err := active.SaveState(ctx, session, checkpoint); !errors.Is(err, storage.ErrAgentLeaseLost) {
			t.Fatalf("expired saver wrote state: %v", err)
		}
		recovered, err := database.ClaimAgentExecution(ctx, organization, claim.ID, time.Minute)
		if err != nil || recovered.LeaseToken == uuid.Nil || recovered.LeaseToken == claim.LeaseToken {
			t.Fatalf("lease not replaced: %v", err)
		}
		if err := active.DeleteSession(ctx, session); !errors.Is(err, storage.ErrAgentLeaseLost) {
			t.Fatalf("replaced saver deleted state: %v", err)
		}
		reader := database.AgentStateSaver(organization, recovered)
		got, writer, err := reader.Restore(ctx)
		if err != nil || got == nil || got.Summary != checkpoint.Summary || writer != claim.ID {
			t.Fatalf("current lease could not restore checkpoint: %v", err)
		}
		sessions, err := reader.ListSessions(ctx)
		if err != nil || len(sessions) != 1 || sessions[0].SessionID != session {
			t.Fatalf("list did not return exactly the bound session: %v", err)
		}

		for _, invalid := range []string{
			`{"session_id": 7, "schema_version": 1}`,
			fmt.Sprintf(`{"session_id": %q, "schema_version": %d}`, session, agent.StateSchemaVersion+1),
			fmt.Sprintf(`{"session_id": %q, "schema_version": 1, "context": [null]}`, session),
		} {
			if _, err := pool.Exec(ctx, `UPDATE agent_checkpoint SET state = $3::jsonb
				WHERE org_id = $1 AND session_id = $2`, organization, session, invalid); err != nil {
				t.Fatal(err)
			}
			if _, err := reader.LoadState(ctx, session); !errors.Is(err, storage.ErrAgentCheckpoint) {
				t.Fatalf("invalid checkpoint accepted: %v", err)
			}
		}
		if err := reader.DeleteSession(ctx, session); err != nil {
			t.Fatal(err)
		}
		if got, err := reader.LoadState(ctx, session); err != nil || got != nil {
			t.Fatalf("deleted checkpoint was not absent: %v", err)
		}
	})

	t.Run("same client Message is admitted once and Conversation claims follow the head", func(t *testing.T) {
		ctx := t.Context()
		chat := openConversation(t, database, organization, "checkout")
		clientID := uuid.New()
		input := conversation.NewMessage{Text: "What changed?"}
		type answer struct {
			execution storage.AgentExecution
			err       error
		}
		start := make(chan struct{})
		admitted := make(chan answer, 2)
		for range 2 {
			go func() {
				<-start
				execution, err := database.AdmitAgentChat(ctx, principal, organization, chat.ID, clientID, input)
				admitted <- answer{execution, err}
			}()
		}
		close(start)
		first, second := <-admitted, <-admitted
		if first.err != nil || second.err != nil || first.execution.ID != second.execution.ID {
			t.Fatalf("identical retries did not return the same execution: %v, %v", first.err, second.err)
		}
		pool, err := poolForTest(database, organization)
		if err != nil {
			t.Fatal(err)
		}
		var mutations int
		err = pool.QueryRow(ctx, "SELECT count(*) FROM audit_event WHERE org_id = $1 AND target_id = $2 AND action = $3",
			organization, chat.ID.String(), audit.ActionConversationMessage).Scan(&mutations)
		if err != nil || mutations != 1 {
			t.Fatalf("identical retry produced %d message mutation audit events: %v", mutations, err)
		}

		detail, err := database.ConversationDetail(ctx, organization, chat.ID, 10)
		if err != nil || len(detail.Messages) != 1 || detail.Messages[0].Sequence != first.execution.MessageSequence {
			t.Fatalf("identical retries did not produce exactly one Message: %v", err)
		}
		if _, err := database.AdmitAgentChat(ctx, principal, organization, chat.ID, clientID,
			conversation.NewMessage{Text: "Different question"}); !errors.Is(err, storage.ErrClientMessageConflict) {
			t.Fatalf("different input reused the client ID: %v", err)
		}

		opened, err := database.OpenQueuedSlackTurn(ctx, time.Hour, 8)
		if err != nil || opened {
			t.Fatalf("legacy worker admitted a Message owned by AgentScope: opened=%v, error=%v", opened, err)
		}

		later, err := database.AdmitAgentChat(ctx, principal, organization, chat.ID, uuid.New(),
			conversation.NewMessage{Text: "What should we check next?"})
		if err != nil {
			t.Fatal(err)
		}
		start = make(chan struct{})
		claimed := make(chan answer, 2)
		for _, id := range []uuid.UUID{first.execution.ID, later.ID} {
			go func() {
				<-start
				execution, err := database.ClaimAgentExecution(ctx, organization, id, time.Minute)
				claimed <- answer{execution, err}
			}()
		}
		close(start)
		head, refused := <-claimed, <-claimed
		if head.err != nil {
			head, refused = refused, head
		}
		if head.err != nil || head.execution.ID != first.execution.ID ||
			!errors.Is(refused.err, storage.ErrAgentExecutionUnavailable) {
			t.Fatalf("head-only claiming failed: %v, %v", head.err, refused.err)
		}
		if _, err := pool.Exec(ctx, `UPDATE agent_execution SET status = 'needs_input',
			lease_token = NULL, lease_expires_at = NULL WHERE org_id = $1 AND execution_id = $2`,
			organization, head.execution.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := database.ClaimAgentExecution(ctx, organization, later.ID, time.Minute); !errors.Is(err, storage.ErrAgentExecutionUnavailable) {
			t.Fatalf("later Message advanced while the head awaited input: %v", err)
		}
	})
}
