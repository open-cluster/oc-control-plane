package storage_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-cluster/oc-control-plane/internal/ai"
	"github.com/open-cluster/oc-control-plane/internal/app"
	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/conversation"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	storage "github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func TestAgentRecoveryKeepsLastCompletedConversation(t *testing.T) {
	database, org, _ := twoOrganizationsInOneDatabase(t)
	principal := executionOwner(t, database, org)
	chat := openConversation(t, database, org, "Recover interrupted work")
	ctx := t.Context()
	admitAndClaim := func(input string) storage.AgentExecution {
		t.Helper()
		queued, err := database.AdmitAgentChat(ctx, principal, org, chat.ID, uuid.New(), conversation.NewMessage{Text: input})
		if err != nil {
			t.Fatal(err)
		}
		claimed, err := database.ClaimAgentExecution(ctx, org, queued.ID, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		return claimed
	}
	first := admitAndClaim("Remember the checkout port")
	snapshot := &agent.AgentState{SchemaVersion: agent.StateSchemaVersion, SessionID: first.SessionID(), Summary: "Port 8443"}
	if err := database.AgentStateSaver(org, first).SaveState(ctx, first.SessionID(), snapshot); err != nil {
		t.Fatal(err)
	}
	if err := database.FinishAgentExecution(ctx, org, first, storage.AgentExecutionCompleted, "Port saved.", nil, investigation.Usage{}); err != nil {
		t.Fatal(err)
	}
	interrupted := admitAndClaim("Inspect another service")
	snapshot.Summary = "Unfinished work"
	if err := database.AgentStateSaver(org, interrupted).SaveState(ctx, interrupted.SessionID(), snapshot); err != nil {
		t.Fatal(err)
	}
	if err := database.SaveChatDraft(ctx, org, interrupted, "An unfinished reply"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		state, err := database.CancelChatExecution(ctx, principal, org, chat.ID, interrupted.ID)
		if err != nil || state.Status != "cancelled" || state.DraftText != "" || state.Message != nil {
			t.Fatalf("cancelled execution = %+v: %v", state, err)
		}
	}
	if err := database.SaveChatDraft(ctx, org, interrupted, "A stale reply"); !errors.Is(err, storage.ErrAgentLeaseLost) {
		t.Fatalf("draft write after cancellation = %v", err)
	}
	state, err := database.CancelChatExecution(ctx, principal, org, chat.ID, first.ID)
	if err != nil || state.Status != "completed" || state.Message == nil || state.Message.Text != "Port saved." {
		t.Fatalf("cancel after completion = %+v: %v", state, err)
	}
	pool, err := poolForTest(database, org)
	if err != nil {
		t.Fatal(err)
	}
	var audits int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_event
		WHERE org_id = $1 AND target_id = $2 AND action = 'conversation.execution-cancelled'`,
		org, chat.ID.String()).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("cancellation recorded %d audit entries: %v", audits, err)
	}
	next := admitAndClaim("What was the checkout port?")
	restored, writer, err := database.AgentStateSaver(org, next).Restore(ctx)
	if err != nil || writer != uuid.Nil || restored == nil || restored.Summary != "Port 8443" {
		t.Fatalf("completed context was replaced by the canceled turn, writer %s: %v", writer, err)
	}
}

func TestAgentCoordinatorCommitsReply(t *testing.T) {
	database, org, _ := twoOrganizationsInOneDatabase(t)
	pool, err := poolForTest(database, org)
	if err != nil {
		t.Fatal(err)
	}
	chat := openConversation(t, database, org, "Persist a completed reply")
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	const input = "What port is checkout using?"
	const answer = "Checkout uses port 8443."
	const nextInput = "Repeat the discovered port."
	const nextAnswer = "8443."
	admitted := make(chan storage.AgentExecution, 1)
	disconnected := make(chan struct{})
	principal := executionOwner(t, database, org)
	clientID := uuid.New()
	// The fixture admits through an actual request context. The worker belongs to the process.
	accept := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		execution, err := database.AdmitAgentChat(r.Context(), principal, org, chat.ID, clientID, conversation.NewMessage{Text: input})
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		admitted <- execution
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: accepted\ndata: queued\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(disconnected)
	}))
	t.Cleanup(accept.Close)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, accept.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stream.Body.Close() })
	if stream.StatusCode != http.StatusOK {
		t.Fatalf("Chat admission fixture returned %d", stream.StatusCode)
	}
	execution := <-admitted

	var requests atomic.Int32
	started := make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		number := requests.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil || bytes.Count(body, []byte(input)) != 1 || !bytes.Contains(body, []byte(`"stream":true`)) {
			t.Errorf("expected a single accepted input on the native streaming path: %v", err)
		}
		if number == 2 {
			if !bytes.Contains(body, []byte(answer)) || bytes.Count(body, []byte(nextInput)) != 1 {
				t.Error("queued turn did not restore the completed Conversation")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			writeAgentAnswer(w, nextAnswer)
			return
		}
		close(started)
		select {
		case <-disconnected:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		writeAgentAnswer(w, answer)
	}))
	t.Cleanup(provider.Close)
	modelClient, err := model.NewOpenAIChatModel(model.OpenAIConfig{APIKey: "fixture", BaseURL: provider.URL, Model: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	startFixtureAgentWorker(t, ctx, database, nil, func(_ context.Context, organization uuid.UUID, claimed storage.AgentExecution) (app.ExecutionProfile, error) {
		if organization != org || claimed.ConversationID != chat.ID {
			t.Error("execution went to the wrong Organization or caller")
		}
		return app.ExecutionProfile{Config: ai.Config{Name: "fixture", Model: modelClient, ToolConcurrency: 1}}, nil
	})
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("process never began the accepted work")
	}
	next, err := database.AdmitAgentChat(ctx, principal, org, chat.ID, uuid.New(), conversation.NewMessage{Text: nextInput})
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.Body.Close()
	awaitAgentResult(t, ctx, pool, org, next.ID, nextAnswer, &requests, 2)
	awaitAgentResult(t, ctx, pool, org, execution.ID, answer, &requests, 2)
	retried, err := database.AdmitAgentChat(ctx, principal, org, chat.ID, clientID, conversation.NewMessage{Text: input})
	if err != nil || retried.ID != execution.ID || retried.ReplyMessageSequence <= next.MessageSequence {
		t.Fatalf("retry did not return the original execution and its linked reply: %v", err)
	}
}

func awaitAgentResult(t *testing.T, ctx context.Context, pool *pgxpool.Pool, org, id uuid.UUID, answer string, requests *atomic.Int32, wantCalls int32) {
	t.Helper()
	for {
		var status storage.AgentExecutionStatus
		var count int
		var text string
		if err := pool.QueryRow(ctx, `
			SELECT e.status, count(m.sequence), coalesce(max(m.text), '')
			  FROM agent_execution e LEFT JOIN conversation_message m
			    ON (m.org_id, m.conversation_id, m.sequence) = (e.org_id, e.conversation_id, e.reply_message_sequence) AND m.role = $3
			 WHERE e.org_id = $1 AND e.execution_id = $2 GROUP BY e.status`,
			org, id, int16(conversation.RoleAssistant)).Scan(&status, &count, &text); err != nil {
			t.Fatal(err)
		}
		if status == storage.AgentExecutionCompleted {
			if count != 1 || text != answer || (wantCalls > 0 && requests.Load() != wantCalls) {
				t.Fatalf("terminal row and one Message were not committed together: count %d, text %q, requests %d", count, text, requests.Load())
			}
			break
		}
		if count != 0 || status == storage.AgentExecutionFailed {
			t.Fatalf("unexpected intermediate state: %s, assistant messages %d", status, count)
		}
		select {
		case <-ctx.Done():
			t.Fatal("execution never completed")
		case <-time.After(30 * time.Millisecond):
		}
	}
}

func TestAgentExecutionCannotPublishAfterOwnershipEnds(t *testing.T) {
	database, org, foreignOrg := twoOrganizationsInOneDatabase(t)
	pool, err := poolForTest(database, org)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	for _, change := range []string{"cancel", "reclaim", "delete"} {
		t.Run(change, func(t *testing.T) {
			chat := openConversation(t, database, org, "Fence a stale product result")
			queued, err := database.AdmitAgentChat(ctx, ownerOf(t, org), org, chat.ID, uuid.New(), conversation.NewMessage{Text: "one input"})
			if err != nil {
				t.Fatal(err)
			}
			stale, err := database.ClaimAgentExecution(ctx, org, queued.ID, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			finish := func(owner uuid.UUID) error {
				return database.FinishAgentExecution(ctx, owner, stale, storage.AgentExecutionCompleted, "stale reply", nil, investigation.Usage{})
			}
			if err := finish(foreignOrg); !errors.Is(err, storage.ErrAgentLeaseLost) {
				t.Fatalf("foreign result write error = %v", err)
			}
			switch change {
			case "cancel":
				if _, err := database.CancelChatExecution(ctx, ownerOf(t, foreignOrg), foreignOrg, chat.ID, stale.ID); !errors.Is(err, conversation.ErrUnknown) {
					t.Fatalf("foreign cancellation error = %v", err)
				}
				if _, err := database.CancelChatExecution(ctx, ownerOf(t, org), org, chat.ID, stale.ID); err != nil {
					t.Fatal(err)
				}
			case "reclaim":
				expireAgentClaim(t, pool, org, stale.ID)
				if _, err := database.ClaimAgentExecution(ctx, org, stale.ID, time.Minute); err != nil {
					t.Fatal(err)
				}
			case "delete":
				if _, err := pool.Exec(ctx, "DELETE FROM conversation_message WHERE org_id = $1 AND conversation_id = $2 AND sequence = $3", org, chat.ID, stale.MessageSequence); err != nil {
					t.Fatal(err)
				}
			}
			if err := finish(org); !errors.Is(err, storage.ErrAgentLeaseLost) {
				t.Fatalf("stale result write error = %v", err)
			}
			var count int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversation_message
				WHERE org_id = $1 AND conversation_id = $2 AND role = $3`, org, chat.ID, int16(conversation.RoleAssistant)).Scan(&count); err != nil || count != 0 {
				t.Fatalf("stale Assistant Messages = %d: %v", count, err)
			}
		})
	}
}

func expireAgentClaim(t *testing.T, pool *pgxpool.Pool, org, id uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `UPDATE agent_execution SET lease_expires_at = clock_timestamp() - interval '1 second'
		WHERE org_id = $1 AND execution_id = $2 AND status = 'running'`, org, id); err != nil {
		t.Fatal(err)
	}
}

const agentRecoveryProcessEnv = "OC_TEST_AGENT_RECOVERY_FILE"

func TestAgentRecoveryProcessFixture(t *testing.T) {
	path := os.Getenv(agentRecoveryProcessEnv)
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		DSN, Provider      string
		ContextSize        int
		InvestigationID    uuid.UUID
		InvestigationInput string
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	m, err := model.NewOpenAIChatModel(model.OpenAIConfig{APIKey: "fixture", BaseURL: fixture.Provider, Model: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	prepare := func(_ context.Context, _ uuid.UUID, execution storage.AgentExecution) (app.ExecutionProfile, error) {
		probe := tool.NewFunctionTool("probe", "Read the fixture port", json.RawMessage(`{"type":"object"}`),
			func(ctx context.Context, _ map[string]any) (any, error) {
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, fixture.Provider+"/probe", nil)
				if err != nil {
					return nil, err
				}
				response, err := http.DefaultClient.Do(req)
				if err != nil {
					return nil, err
				}
				defer func() { _ = response.Body.Close() }()
				result, err := io.ReadAll(response.Body)
				return string(result), err
			})
		probe.ReadOnly, probe.ConcurrencySafe = true, true
		profile := app.ExecutionProfile{Config: ai.Config{
			Name: "fixture", Model: m, Toolkit: tool.NewToolkit(probe), ToolConcurrency: 2,
			Context: agent.ContextConfig{ContextSize: fixture.ContextSize},
		}}
		if fixture.InvestigationID != uuid.Nil && execution.InvestigationID == fixture.InvestigationID {
			profile.Input = func(context.Context) (string, error) { return fixture.InvestigationInput, nil }
		}
		return profile, nil
	}
	cfg := config.Config{
		DatabaseDSN: fixture.DSN, HTTPListenAddress: "127.0.0.1:0", PublicURL: "http://127.0.0.1:8081",
		SealingKey: bytes.Repeat([]byte{7}, 32), InvestigationWorkers: 1,
	}
	if err := app.Run(t.Context(), cfg, os.Stderr, app.Options{PrepareExecution: prepare}); err != nil {
		t.Fatal(err)
	}
}

func TestAgentCoordinatorRecoversAcrossProcessCrashes(t *testing.T) {
	dsn := postgresDSN(t)
	database := openDatabaseForTest(t, dsn)
	if _, err := database.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	org := organization(t, "recover-native")
	ensureTestOrganization(t, database, org)
	chat := openConversation(t, database, org, "One recoverable reply")
	pool, err := poolForTest(database, org)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	const input = "read the fixture port"
	const answer = "Checkout is on port 8443."
	execution, err := database.AdmitAgentChat(ctx, executionOwner(t, database, org), org, chat.ID,
		uuid.New(), conversation.NewMessage{Text: input})
	if err != nil {
		t.Fatal(err)
	}

	var modelCalls, toolCalls atomic.Int32
	firstModel := make(chan struct{})
	firstTool := make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/probe" {
			if toolCalls.Add(1) == 1 {
				close(firstTool)
				<-r.Context().Done()
				return
			}
			_, _ = fmt.Fprint(w, "port-result-8443")
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || bytes.Count(body, []byte(input)) != 1 || !bytes.Contains(body, []byte(`"stream":true`)) {
			t.Errorf("recovery repeated the input or left the streaming path: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		switch modelCalls.Add(1) {
		case 1:
			close(firstModel)
			<-r.Context().Done()
		case 2:
			writeAgentCall(w, "probe-1", "probe")

		case 3:
			if !bytes.Contains(body, []byte("port-result-8443")) || !bytes.Contains(body, []byte("probe-1")) {
				t.Error("resumed model request omitted the recovered tool result")
			}
			_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n", answer)
			_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		default:
			t.Error("model was called after the final checkpoint")
		}
	}))
	t.Cleanup(provider.Close)

	guard, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Release()
	_, err = guard.Exec(ctx, `
		CREATE FUNCTION fixture_agent_barrier() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
		    IF TG_TABLE_NAME = 'agent_checkpoint' THEN
		        PERFORM pg_advisory_xact_lock(22801);
		    ELSE
		        PERFORM pg_advisory_xact_lock(22802);
		    END IF;
		    RETURN NEW;
		END $$;
		CREATE TRIGGER pause_initial_checkpoint BEFORE INSERT ON agent_checkpoint
		  FOR EACH ROW EXECUTE FUNCTION fixture_agent_barrier();
		CREATE TRIGGER pause_reply BEFORE INSERT ON conversation_message
		  FOR EACH ROW WHEN (NEW.role = 2) EXECUTE FUNCTION fixture_agent_barrier();
		SELECT pg_advisory_lock(22801)`)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "agent-recovery.json")
	data, err := json.Marshal(struct{ DSN, Provider string }{DSN: dsn, Provider: provider.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	kill := startAgentRecoveryProcess(t, path)
	awaitAgentDatabaseGate(t, ctx, pool, "INSERT INTO agent_checkpoint")
	kill()
	if modelCalls.Load() != 0 {
		t.Fatal("model was called before durable initialization")
	}
	if _, err = guard.Exec(ctx, "SELECT pg_advisory_unlock(22801), pg_advisory_lock(22802)"); err != nil {
		t.Fatal(err)
	}
	expireAgentClaim(t, pool, org, execution.ID)
	kill = startAgentRecoveryProcess(t, path)
	select {
	case <-firstModel:
	case <-ctx.Done():
		t.Fatal("restarted process never reached its initial model call")
	}
	kill()
	expireAgentClaim(t, pool, org, execution.ID)
	kill = startAgentRecoveryProcess(t, path)
	select {
	case <-firstTool:
	case <-ctx.Done():
		t.Fatal("restored process did not execute the pending read")
	}
	kill()
	expireAgentClaim(t, pool, org, execution.ID)
	kill = startAgentRecoveryProcess(t, path)
	awaitAgentDatabaseGate(t, ctx, pool, "INSERT INTO conversation_message")
	kill()
	if toolCalls.Load() != 2 || modelCalls.Load() != 3 {
		t.Fatalf("mid-step recovery used %d model calls and %d tools", modelCalls.Load(), toolCalls.Load())
	}
	if _, err = guard.Exec(ctx, "SELECT pg_advisory_unlock(22802)"); err != nil {
		t.Fatal(err)
	}
	expireAgentClaim(t, pool, org, execution.ID)
	startAgentRecoveryProcess(t, path)
	awaitAgentResult(t, ctx, pool, org, execution.ID, answer, &modelCalls, 3)
}

func TestInvestigationConclusionSurvivesProcessCrashes(t *testing.T) {
	dsn := postgresDSN(t)
	database := openDatabaseForTest(t, dsn)
	if _, err := database.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	org := organization(t, "recover-conclusion")
	ensureTestOrganization(t, database, org)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	owner := executionOwner(t, database, org)
	chat := openConversation(t, database, org, "Checkout latency")
	admit := func(text string) uuid.UUID {
		t.Helper()
		_, err := appendMessageForTest(database, ctx, owner, org, chat.ID, conversation.NewMessage{
			Role: conversation.RoleUser, AuthorID: owner.UserID().String(), AuthorName: "On-call", Text: text,
		})
		if err != nil {
			t.Fatal(err)
		}
		turn, opened, err := openTurnForTest(database, ctx, org, chat.ID, time.Hour)
		if err != nil || !opened {
			t.Fatalf("open Investigation turn: %t, %v", opened, err)
		}
		return turn.InvestigationID
	}
	const summary = "Checkout impact appeared after the deployment."
	const recap = "No cause established."
	initial := strings.Repeat("checkout telemetry context ", 220)
	opened := admit(initial)
	execution := investigationExecutionID(t, database, org, opened)
	pool, err := poolForTest(database, org)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Release()
	if _, err = guard.Exec(ctx, `
		CREATE FUNCTION fixture_conclusion_barrier() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
		    PERFORM pg_advisory_xact_lock(23201);
		    RETURN NEW;
		END $$;
		CREATE TRIGGER pause_conclusion BEFORE UPDATE ON investigation
		  FOR EACH ROW WHEN (NEW.status = 2) EXECUTE FUNCTION fixture_conclusion_barrier();
		SELECT pg_advisory_lock(23201)`); err != nil {
		t.Fatal(err)
	}

	var investigativeCalls, reportCalls, toolCalls, compactions atomic.Int32
	const answer = "The cause remains unconfirmed."
	const followUpAnswer = "The current evidence is unchanged."
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/probe" {
			toolCalls.Add(1)
			_, _ = fmt.Fprint(w, "port-result-8443")
			return
		}
		var request struct {
			Stream   bool
			Messages json.RawMessage
			Tools    []struct {
				Function struct {
					Name       string
					Parameters json.RawMessage
				}
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if request.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			switch investigativeCalls.Add(1) {
			case 1:
				writeAgentCall(w, "recover-report-read", "probe")
				return
			case 3:
				if bytes.Count(request.Messages, []byte(answer)) != 1 || !bytes.Contains(request.Messages, []byte(summary)) ||
					!bytes.Contains(request.Messages, []byte("port-result-8443")) || bytes.Contains(request.Messages, []byte(recap)) ||
					bytes.Contains(request.Messages, []byte("Produce the Investigation Conclusion")) {
					t.Error("restored Conversation lost native results or retained extra finalization text")
				}
			}
			_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", recap)
			return
		}
		if len(request.Tools) == 1 && bytes.Contains(request.Tools[0].Function.Parameters, []byte("task_overview")) {
			compactions.Add(1)
			arguments := `{"task_overview":"` + summary + `","current_state":"","important_discoveries":"","next_steps":"","context_to_preserve":""}`
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"compact","type":"function","function":{"name":"generate_structured_output","arguments":%s}}]},"finish_reason":"tool_calls"}]}`, strconv.Quote(arguments))
			return
		}
		call := reportCalls.Add(1)
		if !bytes.Contains(request.Messages, []byte("port-result-8443")) {
			t.Error("report generation lost the checkpointed Tool result")
		}
		if len(request.Tools) != 1 || request.Tools[0].Function.Name != "generate_structured_output" {
			t.Error("only AgentScope's report schema should be offered in the buffered request")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		visible := answer
		if call == 3 {
			visible = followUpAnswer
		}
		arguments, _ := json.Marshal(investigation.Conclusion{
			Status: investigation.Inconclusive, Summary: visible,
			Impact: investigation.Impact{Summary: "Impact remains unknown."},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"report","type":"function","function":{"name":"generate_structured_output","arguments":%s}}]},"finish_reason":"tool_calls"}]}`, strconv.Quote(string(arguments)))
	}))
	t.Cleanup(provider.Close)
	path := filepath.Join(t.TempDir(), "agent-recovery.json")
	data, err := json.Marshal(map[string]any{
		"DSN": dsn, "Provider": provider.URL, "ContextSize": 512,
		"InvestigationID": opened, "InvestigationInput": initial,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	kill := startAgentRecoveryProcess(t, path)
	awaitAgentDatabaseGate(t, ctx, pool, "UPDATE investigation SET status")
	kill()
	interrupted, err := database.Investigation(ctx, org, opened)
	if err != nil || interrupted.Status != investigation.StatusRunning || interrupted.Conclusion.Summary != "" {
		t.Fatalf("the interrupted transaction exposed an uncommitted report: %+v, %v", interrupted, err)
	}
	if _, err := guard.Exec(ctx, "SELECT pg_advisory_unlock(23201)"); err != nil {
		t.Fatal(err)
	}
	expireAgentClaim(t, pool, org, execution)
	kill = startAgentRecoveryProcess(t, path)
	for {
		result, err := database.Investigation(ctx, org, opened)
		if err != nil {
			t.Fatal(err)
		}
		if result.Status == investigation.StatusConcluded && result.Conclusion.Summary == answer {
			kill()
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("resumed finalization did not publish the pending report: %+v", result)
		case <-time.After(25 * time.Millisecond):
		}
	}
	if investigating, reports, reads := investigativeCalls.Load(), reportCalls.Load(), toolCalls.Load(); investigating != 2 || reports != 2 || reads != 1 {
		t.Fatalf("got %d investigative calls, %d report calls and %d reads, want 2, 2 and 1", investigating, reports, reads)
	}
	if _, err := database.ClaimAgentExecution(ctx, org, execution, time.Minute); !errors.Is(err, storage.ErrAgentExecutionUnavailable) {
		t.Fatalf("execution could be claimed after commit: %v", err)
	}
	terminal, err := database.InvestigationExecutionSnapshot(ctx, org, opened)
	if err != nil || terminal.Status != "completed" || terminal.ConclusionURL != "/api/v1/investigations/"+opened.String() {
		t.Fatalf("expected a committed terminal snapshot, got %+v, %v", terminal, err)
	}
	var visibleCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversation_message
		WHERE org_id = $1 AND conversation_id = $2 AND investigation_id = $3 AND role = 2 AND text = $4`,
		org, chat.ID, opened, answer).Scan(&visibleCount); err != nil || visibleCount != 1 {
		t.Fatalf("expected one accepted visible reply after restart, got %d, %v", visibleCount, err)
	}
	next := admit("What did we learn?")
	startAgentRecoveryProcess(t, path)
	for {
		result, err := database.Investigation(ctx, org, next)
		if err != nil {
			t.Fatal(err)
		}
		if result.Status == investigation.StatusConcluded && result.Conclusion.Summary == followUpAnswer {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("restored Conversation did not complete its follow-up: %+v", result)
		case <-time.After(25 * time.Millisecond):
		}
	}
	if compactions.Load() != 1 || toolCalls.Load() != 1 {
		t.Fatalf("compactions=%d reads=%d, want one each", compactions.Load(), toolCalls.Load())
	}
}

func TestCompactedConversationSurvivesProcessRestart(t *testing.T) {
	dsn := postgresDSN(t)
	database := openDatabaseForTest(t, dsn)
	if _, err := database.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	org := organization(t, "recover-compacted")
	ensureTestOrganization(t, database, org)
	chat := openConversation(t, database, org, "Compaction")
	principal := executionOwner(t, database, org)
	pool, err := poolForTest(database, org)
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithTimeout(t.Context(), time.Minute)
	defer stop()

	const summary = "The operator selected port 8443."
	const answer = "I retained the selected port."
	const followUp = "Which port did we select?"
	input := strings.Repeat("diagnostic context ", 300)
	var generations atomic.Int32
	var compactions atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if bytes.Contains(body, []byte("generate_structured_output")) {
			compactions.Add(1)
			w.Header().Set("Content-Type", "application/json")
			arguments := `{"task_overview":"` + summary + `","current_state":"","important_discoveries":"","next_steps":"","context_to_preserve":""}`
			_, _ = fmt.Fprintf(w, `{"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"compact","type":"function","function":{"name":"generate_structured_output","arguments":%s}}]},"finish_reason":"tool_calls"}]}`, strconv.Quote(arguments))
			return
		}
		number := generations.Add(1)
		if !bytes.Contains(body, []byte(summary)) || bytes.Contains(body, []byte(input)) ||
			number == 2 && !bytes.Contains(body, []byte(followUp)) {
			t.Errorf("turn %d did not use the compacted native context", number)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", answer)
	}))
	defer provider.Close()

	admit := func(text string) uuid.UUID {
		t.Helper()
		work, err := database.AdmitAgentChat(ctx, principal, org, chat.ID, uuid.New(), conversation.NewMessage{Text: text})
		if err != nil {
			t.Fatal(err)
		}
		return work.ID
	}
	path := filepath.Join(t.TempDir(), "agent-recovery.json")
	data, err := json.Marshal(struct {
		DSN, Provider string
		ContextSize   int
	}{DSN: dsn, Provider: provider.URL, ContextSize: 512})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	first := admit(input)
	kill := startAgentRecoveryProcess(t, path)
	awaitAgentResult(t, ctx, pool, org, first, answer, &generations, 1)
	kill()
	second := admit(followUp)
	startAgentRecoveryProcess(t, path)
	awaitAgentResult(t, ctx, pool, org, second, answer, &generations, 2)
	if compactions.Load() != 1 {
		t.Fatalf("native context was compacted %d times, want one", compactions.Load())
	}
	var visible string
	if err := pool.QueryRow(ctx, `SELECT text FROM conversation_message
		WHERE org_id = $1 AND conversation_id = $2 AND sequence = 1`, org, chat.ID).Scan(&visible); err != nil || visible != input {
		t.Fatalf("compaction changed the visible Message: %v", err)
	}
}

func writeConclusionCall(w http.ResponseWriter, report investigation.Conclusion) {
	arguments, _ := json.Marshal(report)
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, "{\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"tool_calls\":[{\"id\":\"report\",\"type\":\"function\",\"function\":{\"name\":\"generate_structured_output\",\"arguments\":%s}}]},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":17,\"completion_tokens\":3,\"total_tokens\":20}}", strconv.Quote(string(arguments)))
}

func startAgentRecoveryProcess(t *testing.T, path string) func() {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestAgentRecoveryProcessFixture$")
	cmd.Env = append(os.Environ(), agentRecoveryProcessEnv+"="+path)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	kill := func() { once.Do(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }) }
	t.Cleanup(kill)
	return kill
}

func awaitAgentDatabaseGate(t *testing.T, ctx context.Context, pool *pgxpool.Pool, statement string) {
	t.Helper()
	for {
		var waiting bool
		err := pool.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity WHERE datname = current_database()
			  AND wait_event = 'advisory' AND strpos(query, $1) > 0)`, statement).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("worker never reached the %s gate", statement)
		case <-time.After(30 * time.Millisecond):
		}
	}
}

func TestAgentChatUsesExistingQueueCapacity(t *testing.T) {
	database, org, _ := twoOrganizationsInOneDatabase(t)
	ctx := t.Context()
	chat := openConversation(t, database, org, "Bound pending work")
	principal := ownerOf(t, org)
	key := uuid.New()
	input := conversation.NewMessage{Text: "pending question"}
	first, err := database.AdmitAgentChat(ctx, principal, org, chat.ID, key, input)
	if err != nil {
		t.Fatal(err)
	}
	for range 99 {
		if _, err := database.AdmitAgentChat(ctx, principal, org, chat.ID, uuid.New(), input); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := database.AdmitAgentChat(ctx, principal, org, chat.ID, uuid.New(), input); !errors.Is(err, conversation.ErrQueueFull) {
		t.Fatalf("new admission over capacity: %v", err)
	}
	retry, err := database.AdmitAgentChat(ctx, principal, org, chat.ID, key, input)
	if err != nil || retry.ID != first.ID {
		t.Fatalf("an identical retry at capacity lost the first admission: %v", err)
	}
}

func TestAgentCoordinatorRejectsIncompleteReplies(t *testing.T) {
	database, org, _ := twoOrganizationsInOneDatabase(t)
	pool, err := poolForTest(database, org)
	if err != nil {
		t.Fatal(err)
	}
	principal := executionOwner(t, database, org)
	for _, fault := range []string{"confirmation", "provider error", "truncated", "iteration limit", "oversize"} {
		t.Run(fault, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			chat := openConversation(t, database, org, fault)
			const firstInput = "attempt failing diagnostic"
			admit := func(text string) storage.AgentExecution {
				t.Helper()
				execution, err := database.AdmitAgentChat(ctx, principal, org, chat.ID, uuid.New(), conversation.NewMessage{Text: text})
				if err != nil {
					t.Fatal(err)
				}
				return execution
			}
			first, next := admit(firstInput), admit("next queued question")
			var requests atomic.Int32
			var forced atomic.Bool
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := requests.Add(1)
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if !bytes.Contains(body, []byte(firstInput)) {
					writeAgentAnswer(w, "next answer")
					return
				}
				switch fault {
				case "confirmation":
					writeAgentCall(w, "confirm", "probe")
				case "provider error":
					w.WriteHeader(http.StatusUnauthorized)
				case "truncated":
					_, _ = fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"content":"unfinished"},"finish_reason":"length"}]}`+"\n\ndata: [DONE]\n\n")
				case "iteration limit":
					if bytes.Contains(body, []byte(`"tool_choice":"none"`)) || !bytes.Contains(body, []byte(`"tools":`)) {
						forced.Store(true)
						writeAgentAnswer(w, "forced summary")
					} else {
						writeAgentCall(w, fmt.Sprintf("probe-%d", call), "probe")
					}
				case "oversize":
					writeAgentAnswer(w, strings.Repeat("x", conversation.MaxMessageTextLength+1))
				}
			}))
			defer provider.Close()
			m, err := model.NewOpenAIChatModel(model.OpenAIConfig{
				APIKey:  "fixture",
				BaseURL: provider.URL,
				Model:   "fixture",
			})
			if err != nil {
				t.Fatal(err)
			}
			startFixtureAgentWorker(t, ctx, database, nil, func(_ context.Context, _ uuid.UUID, _ storage.AgentExecution) (app.ExecutionProfile, error) {
				probe := tool.NewFunctionTool("probe", "Reads the fixture", json.RawMessage(`{"type":"object"}`),
					func(context.Context, map[string]any) (any, error) {
						if fault == "confirmation" {
							t.Error("permission was bypassed")
						}
						return "fixture result", nil
					})
				probe.ReadOnly = fault != "confirmation"
				var permissions *permission.Context
				if fault == "confirmation" {
					permissions = permission.NewContext(permission.ModeDefault)
				}
				return app.ExecutionProfile{
					Config: ai.Config{
						Name:            "fixture",
						Model:           m,
						Toolkit:         tool.NewToolkit(probe),
						Permissions:     permissions,
						ToolConcurrency: 1,
					},
				}, nil
			})
			awaitAgentResult(t, ctx, pool, org, next.ID, "next answer", &requests, 0)
			var status storage.AgentExecutionStatus
			var hasReply bool
			err = pool.QueryRow(ctx, "SELECT status, reply_message_sequence IS NOT NULL FROM agent_execution WHERE org_id = $1 AND execution_id = $2",
				org, first.ID).Scan(&status, &hasReply)
			if err != nil || status != storage.AgentExecutionFailed || hasReply {
				t.Fatalf("incomplete reply persisted as %s, hasReply %t: %v", status, hasReply, err)
			}
			if fault == "iteration limit" && !forced.Load() {
				t.Fatal("the case never reached native forced finalization")
			}
		})
	}
}

func writeAgentAnswer(w http.ResponseWriter, answer string) {
	_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n", answer)
	_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
}

func startFixtureAgentWorker(t *testing.T, ctx context.Context, db *storage.Database, wake <-chan struct{}, prepare app.PrepareExecution) <-chan struct{} {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		app.RunExecutions(ctx, db, 1, wake, prepare, nil)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("coordinator failed to stop")
		}
	})
	return done
}

func TestAgentCoordinatorFinishesAndCancelsInvestigationThroughProductStorage(t *testing.T) {
	database, org, _ := twoOrganizationsInOneDatabase(t)
	pool, err := poolForTest(database, org)
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithTimeout(t.Context(), 15*time.Second)
	defer stop()
	principal := executionOwner(t, database, org)
	admit := func(subject string) (investigation.Investigation, uuid.UUID) {
		t.Helper()
		entity, err := database.CreateInvestigation(ctx, principal, org, investigation.NewInvestigation{
			Subject: subject, WindowFrom: time.Now().Add(-time.Hour), WindowUntil: time.Now(),
		}, 8)
		if err != nil {
			t.Fatal(err)
		}
		return entity, investigationExecutionID(t, database, org, entity.ID)
	}
	abandoned, pending := admit("Cancelled checkout")
	claim, err := database.ClaimAgentExecution(ctx, org, pending, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = database.CancelInvestigation(ctx, principal, org, abandoned.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.FinishAgentExecution(ctx, org, claim, storage.AgentExecutionCompleted, "",
		&investigation.Conclusion{Status: investigation.Inconclusive, Summary: "stale"}, investigation.Usage{}); !errors.Is(err, storage.ErrAgentLeaseLost) {
		t.Fatalf("canceled Investigation accepted a terminal write: %v", err)
	}
	_, unsupported := admit("Unsupported checkout")
	claim, err = database.ClaimAgentExecution(ctx, org, unsupported, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := investigation.ParseConclusion([]byte(`{"status":"inconclusive","summary":"Checkout errors","impact":{"summary":"Impact cited by ordinal","runRefs":[1]}}`)); err == nil {
		t.Fatal("native finalization accepted an ordinal Impact citation")
	}
	conclusion := &investigation.Conclusion{
		Status:  investigation.Inconclusive,
		Summary: "Impact cited by ordinal",
		Impact: investigation.Impact{
			Summary: "Checkout errors",
		},
	}
	conclusion.Status = investigation.SupportedExplanation
	if err := database.FinishAgentExecution(ctx, org, claim, storage.AgentExecutionCompleted, "",
		conclusion, investigation.Usage{}); err == nil {
		t.Fatal("native finalization accepted a supported explanation without an explanatory Finding")
	}
	conclusion.Status = investigation.HistoricalAnswerOnly
	if err := database.FinishAgentExecution(ctx, org, claim, storage.AgentExecutionCompleted, "",
		conclusion, investigation.Usage{}); !errors.Is(err, investigation.ErrInvalidConclusion) {
		t.Fatalf("standalone Investigation accepted answer_only: %v", err)
	}
	conclusion.Status = ""
	if err := database.FinishAgentExecution(ctx, org, claim, storage.AgentExecutionCompleted, "",
		conclusion, investigation.Usage{}); !errors.Is(err, investigation.ErrInvalidConclusion) {
		t.Fatalf("standalone Investigation accepted an unassessed reply: %v", err)
	}
	if err := database.FinishAgentExecution(ctx, org, claim, storage.AgentExecutionFailed, "", nil, investigation.Usage{}); err != nil {
		t.Fatal(err)
	}
	entity, work := admit("Inspect checkout")
	rejected, rejectedWork := admit("Reject checkout evidence")

	const answer = "No confirmed cause was found."
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if !strings.Contains(string(body), "generate_structured_output") {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"No cause established.\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		report := investigation.Conclusion{
			Status: investigation.Inconclusive, Summary: answer,
			Impact: investigation.Impact{Summary: "Impact remains unknown."},
			Actions: []investigation.ActionProposal{{
				Title: "Collect request latency", Rationale: "Request timing is unavailable.",
				Verification: "Confirm latency metrics cover the Incident window.",
			}},
		}
		if strings.Contains(string(body), "Reject checkout evidence") {
			report.Status = investigation.SupportedExplanation
			report.Findings = []investigation.Finding{{
				Kind: investigation.FindingCause, Statement: "The pool exhausted its connections.",
				Mechanism:    "The queue starved requests of database connections.",
				EvidenceRefs: []investigation.EvidenceRef{{ToolRunID: uuid.New()}},
			}}
		}
		writeConclusionCall(w, report)
	}))
	defer provider.Close()
	m, err := model.NewOpenAIChatModel(model.OpenAIConfig{APIKey: "fixture", BaseURL: provider.URL, Model: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	startFixtureAgentWorker(t, t.Context(), database, nil, func(_ context.Context, owner uuid.UUID, execution storage.AgentExecution) (app.ExecutionProfile, error) {
		if owner != org || (execution.InvestigationID != entity.ID && execution.InvestigationID != rejected.ID) {
			t.Error("worker resolved a canceled or foreign Investigation")
		}
		return app.ExecutionProfile{
			Config: ai.Config{Name: "fixture", Model: m, ToolConcurrency: 1},
		}, nil
	})
	for {
		var status storage.AgentExecutionStatus
		var result investigation.Status
		var summary string
		var legacyEvents int
		var usage investigation.Usage
		err := pool.QueryRow(ctx, `SELECT e.status, i.status, coalesce(i.conclusion->>'summary', ''), i.spend_input_tokens, i.spend_output_tokens,
		   (SELECT count(*) FROM investigation_event v WHERE v.org_id = e.org_id AND v.investigation_id = e.investigation_id)
		    FROM agent_execution e JOIN investigation i USING (org_id, investigation_id)
		   WHERE e.org_id = $1 AND e.execution_id = $2`, org, work).Scan(&status, &result, &summary, &usage.InputTokens, &usage.OutputTokens, &legacyEvents)
		if err != nil {
			t.Fatal(err)
		}
		if status == storage.AgentExecutionCompleted {
			if result != investigation.StatusConcluded || summary != answer || legacyEvents != 0 || usage != (investigation.Usage{InputTokens: 17, OutputTokens: 3}) {
				t.Fatalf("expected an atomic Investigation conclusion, got %s, %q, %d legacy events and usage %+v", result, summary, legacyEvents, usage)
			}
			break
		}
		if result != investigation.StatusRunning || status == storage.AgentExecutionFailed || summary != "" {
			t.Fatalf("unexpected intermediate result: %s, %s, %q", status, result, summary)
		}
		select {
		case <-ctx.Done():
			t.Fatal("Investigation never completed")
		case <-time.After(30 * time.Millisecond):
		}
	}
	for {
		result, err := database.Investigation(ctx, org, rejected.ID)
		if err != nil {
			t.Fatal(err)
		}
		if result.Status == investigation.StatusConcluded {
			t.Fatal("unavailable evidence supported a completed Investigation")
		}
		if result.Status == investigation.StatusFailed {
			var status storage.AgentExecutionStatus
			if err := pool.QueryRow(ctx, `SELECT status FROM agent_execution WHERE org_id = $1 AND execution_id = $2`,
				org, rejectedWork).Scan(&status); err != nil || status != storage.AgentExecutionFailed {
				t.Fatalf("evidence rejection did not terminalize execution: %v, %v", status, err)
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("the rejected Investigation did not terminate")
		case <-time.After(30 * time.Millisecond):
		}
	}
}

func TestAgentCoordinatorCancelsDuringPreparation(t *testing.T) {
	db, org, _ := twoOrganizationsInOneDatabase(t)
	ctx := t.Context()
	chat := openConversation(t, db, org, "Lease through preparation")
	execution, err := db.AdmitAgentChat(ctx, ownerOf(t, org), org, chat.ID, uuid.New(), conversation.NewMessage{Text: "question"})
	if err != nil {
		t.Fatal(err)
	}
	started, canceled := make(chan struct{}), make(chan struct{})
	startFixtureAgentWorker(t, ctx, db, nil, func(ctx context.Context, _ uuid.UUID, _ storage.AgentExecution) (app.ExecutionProfile, error) {
		close(started)
		<-ctx.Done()
		close(canceled)
		return app.ExecutionProfile{}, ctx.Err()
	})
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("profile preparation never began")
	}
	if _, err := db.CancelChatExecution(ctx, ownerOf(t, org), org, chat.ID, execution.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-canceled:
	case <-time.After(6 * time.Second):
		t.Fatal("durable cancellation did not reach profile preparation")
	}
}

func TestAgentCoordinatorKeepsLeaseDuringToolCleanup(t *testing.T) {
	db, org, _ := twoOrganizationsInOneDatabase(t)
	pool, err := poolForTest(db, org)
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithTimeout(t.Context(), 20*time.Second)
	defer stop()
	chat := openConversation(t, db, org, "Lease during cleanup")
	work, err := db.AdmitAgentChat(ctx, ownerOf(t, org), org, chat.ID, uuid.New(), conversation.NewMessage{Text: "read status"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE agent_execution SET started_at = clock_timestamp() - interval '29 minutes 54 seconds'
		WHERE org_id = $1 AND execution_id = $2`, org, work.ID); err != nil {
		t.Fatal(err)
	}
	cleaning, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeAgentCall(w, "read-1", "slow_read")
	}))
	defer provider.Close()
	m, err := model.NewOpenAIChatModel(model.OpenAIConfig{APIKey: "fixture", BaseURL: provider.URL, Model: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	done := startFixtureAgentWorker(t, ctx, db, nil, func(_ context.Context, _ uuid.UUID, _ storage.AgentExecution) (app.ExecutionProfile, error) {
		read := tool.NewFunctionTool("slow_read", "Read and close the fixture", json.RawMessage(`{"type":"object"}`),
			func(ctx context.Context, _ map[string]any) (any, error) {
				<-ctx.Done()
				close(cleaning)
				<-release
				return nil, ctx.Err()
			})
		read.ReadOnly, read.ConcurrencySafe = true, true
		return app.ExecutionProfile{Config: ai.Config{Name: "fixture", Model: m, Toolkit: tool.NewToolkit(read), ToolConcurrency: 1}}, nil
	})
	select {
	case <-cleaning:
	case <-ctx.Done():
		t.Fatal("the restored work budget did not end the tool context")
	}
	leaseExpiry := func() time.Time {
		var expiry time.Time
		err := pool.QueryRow(ctx, "SELECT lease_expires_at FROM agent_execution WHERE org_id = $1 AND execution_id = $2",
			org, work.ID).Scan(&expiry)
		if err != nil {
			t.Fatal(err)
		}
		return expiry
	}
	expires := leaseExpiry()
	deadline := time.After(5 * time.Second)
	for !leaseExpiry().After(expires) {
		select {
		case <-deadline:
			t.Fatal("worker ceased renewing while a tool still held its slot")
		case <-time.After(50 * time.Millisecond):
		}
	}
	stop()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("process waited indefinitely for a canceled tool to clean up")
	}
}

func writeAgentCall(w http.ResponseWriter, id, name string) {
	_, _ = fmt.Fprintf(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":%q,"type":"function","function":{"name":%q,"arguments":"{}"}}]},"finish_reason":null}]}`+"\n\n", id, name)
	_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
}

func TestAgentClaimSkipsLockedConversation(t *testing.T) {
	database, org, _ := twoOrganizationsInOneDatabase(t)
	pool, err := poolForTest(database, org)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	principal := ownerOf(t, org)
	var busy []uuid.UUID
	for range 16 {
		chat := openConversation(t, database, org, "busy Conversation")
		busy = append(busy, chat.ID)
		if _, err := database.AdmitAgentChat(ctx, principal, org, chat.ID, uuid.New(),
			conversation.NewMessage{Text: "check status"}); err != nil {
			t.Fatal(err)
		}
	}
	ready := openConversation(t, database, org, "ready Conversation")
	if _, err := database.AdmitAgentChat(ctx, principal, org, ready.ID, uuid.New(),
		conversation.NewMessage{Text: "check status"}); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM conversation WHERE org_id = $1 AND conversation_id = ANY($2::uuid[]) FOR UPDATE`, org, busy); err != nil {
		t.Fatal(err)
	}
	claimCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	owner, execution, err := database.ClaimNextAgentExecution(claimCtx, time.Minute)
	if err != nil || owner != org || execution.ConversationID != ready.ID {
		t.Fatalf("locked Conversation stalled unrelated execution: %s, %v", execution.ConversationID, err)
	}
}
