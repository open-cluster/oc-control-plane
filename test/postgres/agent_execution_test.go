package storage_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/conversation"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	storage "github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func TestAgentExecution(t *testing.T) {
	database, organization, foreignOrg := twoOrganizationsInOneDatabase(t)
	principal := ownerOf(t, organization)

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
		if err := database.EndAgentExecution(ctx, organization, first, storage.AgentExecutionCompleted); err != nil {
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
		if _, err := database.AdmitAgentInvestigation(ctx, foreignOrg, created.ID); !errors.Is(err, storage.ErrAgentExecutionUnavailable) {
			t.Fatalf("foreign Investigation admitted: %v", err)
		}
		queued, err := database.AdmitAgentInvestigation(ctx, organization, created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := database.ClaimAgentExecution(ctx, foreignOrg, queued.ID, time.Minute); !errors.Is(err, storage.ErrAgentExecutionUnavailable) {
			t.Fatalf("foreign execution claimed: %v", err)
		}
		claim, err := database.ClaimAgentExecution(ctx, organization, queued.ID, time.Minute)
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

		opened, err := database.OpenQueuedConversationTurn(ctx, time.Hour, 8)
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
		if err := database.EndAgentExecution(ctx, organization, head.execution, storage.AgentExecutionNeedsInput); err != nil {
			t.Fatal(err)
		}
		if _, err := database.ClaimAgentExecution(ctx, organization, later.ID, time.Minute); !errors.Is(err, storage.ErrAgentExecutionUnavailable) {
			t.Fatalf("later Message advanced while the head awaited input: %v", err)
		}
	})
}
