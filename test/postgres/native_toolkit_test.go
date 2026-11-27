package storage_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-cluster/oc-control-plane/internal/ai"
	"github.com/open-cluster/oc-control-plane/internal/app"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/conversation"
	executionview "github.com/open-cluster/oc-control-plane/internal/execution"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/integrations/prometheus"
	"github.com/open-cluster/oc-control-plane/internal/seal"
	storage "github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

const toolkitCredential = "fixture-provider-credential"

func TestNativeToolkitCommitsFastReadBeforeTheBatchCompletes(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	started := make(chan string, 3)
	fast, slow := make(chan struct{}), make(chan struct{})
	var active atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+toolkitCredential {
			t.Error("provider did not receive the current credential")
		}
		if active.Add(1) > 2 {
			t.Error("more than two reads dispatched concurrently")
		}
		defer active.Add(-1)
		query := r.FormValue("query")
		started <- query
		switch query {
		case "fast":
			select {
			case <-fast:
			case <-r.Context().Done():
				return
			}
		case "slow":
			select {
			case <-slow:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"service":"checkout"},"value":[1,"1"]}]}}`)
	}))
	t.Cleanup(provider.Close)
	database, pool, org, principal, sealer, catalog, _ := toolkitFixture(t, provider.URL)
	chat := openConversation(t, database, org, "Read metrics")
	execution, err := database.AdmitAgentChat(ctx, principal, org, chat.ID, uuid.New(), conversation.NewMessage{Text: "Read three metrics."})
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	secondCall := make(chan struct{})
	releaseSecond := make(chan struct{})
	defer func() {
		select {
		case <-releaseSecond:
		default:
			close(releaseSecond)
		}
	}()
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error("reading the scripted model request failed")
			return
		}
		if strings.Contains(string(body), toolkitCredential) {
			t.Error("a provider credential crossed the model boundary")
		}
		if requests.Add(1) == 1 {
			_, _ = fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"content":"Checking metrics."},"finish_reason":null}]}`+"\n\n")
			for index, name := range []string{"slow", "fast", "third"} {
				args := fmt.Sprintf(`{"input":{"query":%q,"mode":"instant"}}`, name)
				_, _ = fmt.Fprintf(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":%d,"id":%q,"type":"function","function":{"name":"prometheus_query","arguments":%q}}]},"finish_reason":null}]}`+"\n\n", index, name, args)
			}
			_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		if !strings.Contains(string(body), "checkout") {
			t.Error("the model did not receive the committed observation")
		}
		close(secondCall)
		select {
		case <-releaseSecond:
		case <-r.Context().Done():
			return
		}
		writeAgentAnswer(w, "Reads completed.")
	}))
	t.Cleanup(modelServer.Close)
	client, err := model.NewOpenAIChatModel(model.OpenAIConfig{APIKey: "fixture", BaseURL: modelServer.URL, Model: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	startFixtureAgentWorker(t, ctx, database, nil, func(ctx context.Context, org uuid.UUID, claim storage.AgentExecution) (app.ExecutionProfile, error) {
		profile := app.ExecutionProfile{Config: ai.Config{Name: "fixture", Model: client}}
		err := profile.UseIntegrationTools(ctx, database, catalog, sealer, org, claim)
		return profile, err
	})
	for range 2 {
		select {
		case query := <-started:
			if query != "slow" && query != "fast" {
				t.Fatalf("the first batch exceeded its width: %s", query)
			}
		case <-ctx.Done():
			t.Fatal("independent reads did not overlap")
		}
	}
	close(fast)
	var id uuid.UUID
	var observation string
	for {
		var complete bool
		err := pool.QueryRow(ctx, `
			SELECT tool_run_id, coalesce(outcome = 1, false), coalesce(observation_text, '')
			  FROM investigation_tool_run
			 WHERE org_id = $1 AND execution_id = $2 AND arguments ->> 'query' = 'fast'`,
			org, execution.ID).Scan(&id, &complete, &observation)
		if err != nil {
			t.Fatal(err)
		}
		if complete {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("the fast Tool Run did not commit while the slow read was blocked")
		case <-time.After(20 * time.Millisecond):
		}
	}
	if id == uuid.Nil || !strings.Contains(observation, "checkout") || strings.Contains(observation, toolkitCredential) || requests.Load() != 1 {
		t.Fatal("the fast observation was not committed safely before native batch publication")
	}
	snapshot, err := database.ChatExecutionSnapshot(ctx, org, chat.ID, execution.ID)
	if err != nil || snapshot.DraftText != "Checking metrics." || !slices.ContainsFunc(snapshot.Tools, func(run executionview.ToolActivity) bool {
		return run.ToolRunID == id && run.Status == "succeeded" && run.FinishedAt != nil
	}) || !slices.ContainsFunc(snapshot.Tools, func(run executionview.ToolActivity) bool {
		return run.ToolRunID != id && run.Status == "running" && run.FinishedAt == nil
	}) {
		t.Fatalf("snapshot did not expose the fast result alongside the blocked read: %+v, %v", snapshot.Tools, err)
	}
	close(slow)
	select {
	case <-secondCall:
	case <-ctx.Done():
		t.Fatal("the model did not start a new call after Tool completion")
	}
	for {
		current, err := database.ChatExecutionSnapshot(ctx, org, chat.ID, execution.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.DraftText == "" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("the next native ModelCallStart did not clear the earlier provisional text")
		case <-time.After(20 * time.Millisecond):
		}
	}
	close(releaseSecond)
	awaitAgentResult(t, ctx, pool, org, execution.ID, "Reads completed.", &requests, 2)
}

func toolkitFixture(t *testing.T, endpoint string) (*storage.Database, *pgxpool.Pool, uuid.UUID, authz.Principal, seal.Sealer, integrations.Catalog, integrations.Integration) {
	t.Helper()
	database, org := migratedDatabase(t)
	ctx := t.Context()
	pool, err := poolForTest(database, org)
	if err != nil {
		t.Fatal(err)
	}
	principal := ownerOf(t, org)
	if _, err := pool.Exec(ctx, `INSERT INTO app_user (user_id, issuer, subject, email)
	    VALUES ($1, 'toolkit-test', $2, 'operator@example.test')`, principal.UserID(), principal.UserID().String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO organization_membership (org_id, user_id, role)
	    VALUES ($1, $2, $3)`, org, principal.UserID(), string(authz.Admin)); err != nil {
		t.Fatal(err)
	}
	sealer, err := seal.New(bytes.Repeat([]byte{1}, seal.KeyLength))
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	credential, err := sealer.Seal(toolkitCredential, integrations.CredentialBinding(id))
	if err != nil {
		t.Fatal(err)
	}
	integration, err := database.CreateIntegration(ctx, principal, org, integrations.NewIntegration{
		ID: id, Provider: "prometheus", Name: "Metrics",
		Configuration:    map[string]any{"url": endpoint},
		CredentialSealed: credential,
		Verification: &integrations.Verification{
			Status: integrations.StatusVerified, Grants: []string{"prometheus:query"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := integrations.NewCatalog(prometheus.Definition())
	if err != nil {
		t.Fatal(err)
	}
	return database, pool, org, principal, sealer, catalog, integration
}

func TestNativeToolkitRevalidatesAuthorityAndFailsClosed(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	credentials := make(chan string, 8)
	held, release := make(chan struct{}, 2), make(chan struct{}, 2)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		credentials <- r.Header.Get("Authorization")
		switch r.FormValue("query") {
		case "failed":
			http.Error(w, toolkitCredential, http.StatusServiceUnavailable)
			return
		case "held", "revoked":
			held <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	}))
	t.Cleanup(provider.Close)
	database, pool, org, principal, sealer, catalog, integration := toolkitFixture(t, provider.URL)
	if _, err := database.CreateIntegration(ctx, principal, org, integrations.NewIntegration{
		Provider:      integration.Provider,
		Name:          "Another metrics source",
		Configuration: integration.Configuration,
		Verification: &integrations.Verification{
			Status: integrations.StatusVerified, Grants: []string{"prometheus:query"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	chat := openConversation(t, database, org, "Verify current access")
	queued, err := database.AdmitAgentChat(ctx, principal, org, chat.ID, uuid.New(), conversation.NewMessage{Text: "Read metrics."})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := database.ClaimAgentExecution(ctx, org, queued.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var profile app.ExecutionProfile
	if err := profile.UseIntegrationTools(ctx, database, catalog, sealer, org, claim); err != nil {
		t.Fatal(err)
	}
	type toolReply struct {
		body   string
		failed bool
		err    error
	}
	call := func(query string) toolReply {
		response, err := profile.Toolkit.CallTool(ctx, "prometheus_query", map[string]any{
			"input":         map[string]any{"query": query, "mode": "instant"},
			"integrationId": integration.ID.String(),
		})
		if err != nil {
			return toolReply{err: err}
		}
		raw, err := json.Marshal(response)
		return toolReply{body: string(raw), failed: response.State == message.ToolResultError, err: err}
	}
	checkFailure := func(reply toolReply, reason string) {
		t.Helper()
		if reply.err != nil || !reply.failed || !strings.Contains(reply.body, reason) ||
			strings.Contains(reply.body, "[Tool Run ") || strings.Contains(reply.body, toolkitCredential) {
			t.Fatalf("expected safe Tool error %q, got %v", reason, reply.err)
		}
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	assertNoDispatch := func() {
		t.Helper()
		select {
		case <-credentials:
			t.Fatal("a rejected Tool request reached the provider")
		default:
		}
	}
	setRole := func(role authz.Role) {
		exec(`UPDATE organization_membership SET role = $3 WHERE org_id = $1 AND user_id = $2`,
			org, principal.UserID(), string(role))
	}
	response, err := profile.Toolkit.CallTool(ctx, "prometheus_query", map[string]any{
		"input": map[string]any{"query": "ambiguous", "mode": "instant"},
	})
	if err != nil || response.State != message.ToolResultError {
		t.Fatal("multiple eligible Integrations did not require explicit selection")
	}
	assertNoDispatch()
	exec(`UPDATE app_user SET disabled_at = clock_timestamp() WHERE user_id = $1`, principal.UserID())
	checkFailure(call("disabled user"), "no longer authorized")
	assertNoDispatch()
	exec(`UPDATE app_user SET disabled_at = NULL WHERE user_id = $1`, principal.UserID())
	exec(`UPDATE integration SET verification_grants = '{}' WHERE org_id = $1 AND integration_id = $2`, org, integration.ID)
	checkFailure(call("grants"), "no longer authorized")
	assertNoDispatch()
	exec(`UPDATE integration SET verification_grants = ARRAY['prometheus:query'] WHERE org_id = $1 AND integration_id = $2`, org, integration.ID)
	exec(`ALTER TABLE audit_event ADD CONSTRAINT toolkit_test_audit CHECK (action <> 'integration.credential.unsealed') NOT VALID`)
	checkFailure(call("audit"), "could not be audited")
	assertNoDispatch()
	exec(`ALTER TABLE audit_event DROP CONSTRAINT toolkit_test_audit`)
	exec(`CREATE FUNCTION toolkit_test_revoke_grants() RETURNS trigger LANGUAGE plpgsql AS $$
	    BEGIN
	        UPDATE integration SET verification_grants = '{}'
	         WHERE org_id = NEW.org_id AND integration_id = NEW.target_id::uuid;
	        RETURN NEW;
	    END $$`)
	exec(`CREATE TRIGGER toolkit_test_revoke_grants BEFORE INSERT ON audit_event
	    FOR EACH ROW WHEN (NEW.action = 'integration.credential.unsealed')
	    EXECUTE FUNCTION toolkit_test_revoke_grants()`)
	checkFailure(call("revoked at admission"), "could not admit the Tool Run")
	assertNoDispatch()
	exec(`DROP TRIGGER toolkit_test_revoke_grants ON audit_event`)
	exec(`UPDATE integration SET verification_grants = ARRAY['prometheus:query'] WHERE org_id = $1 AND integration_id = $2`, org, integration.ID)

	const rotated = "fixture-rotated-credential"
	sealed, err := sealer.Seal(rotated, integrations.CredentialBinding(integration.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ReplaceIntegrationCredential(ctx, principal, org, integration, integrations.IntegrationUpdate{}, sealed,
		integrations.Verification{Status: integrations.StatusVerified, Grants: []string{"prometheus:query"}}, nil); err != nil {
		t.Fatal(err)
	}
	failure := call("failed")
	checkFailure(failure, "did not complete")
	if got := <-credentials; got != "Bearer "+rotated {
		t.Fatal("the existing Toolkit did not use the rotated credential")
	}
	var failed uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT tool_run_id FROM investigation_tool_run
		WHERE org_id = $1 AND execution_id = $2 AND outcome = 2 AND arguments ->> 'query' = 'failed'`,
		org, claim.ID).Scan(&failed); err != nil || !strings.Contains(failure.body, failed.String()) {
		t.Fatalf("provider failure did not return the durable failed Tool Run ID: %s, %v", failed, err)
	}
	exec(`ALTER TABLE investigation_tool_run ADD CONSTRAINT toolkit_test_persistence CHECK (outcome <> 1) NOT VALID`)
	checkFailure(call("persistence"), "observation could not be recorded")
	<-credentials
	exec(`ALTER TABLE investigation_tool_run DROP CONSTRAINT toolkit_test_persistence`)

	done := make(chan toolReply, 1)
	go func() { done <- call("revoked") }()
	select {
	case <-held:
	case <-ctx.Done():
		t.Fatal("the read under current Membership did not start")
	}
	setRole(authz.Viewer)
	release <- struct{}{}
	select {
	case reply := <-done:
		checkFailure(reply, "observation could not be recorded")
	case <-ctx.Done():
		t.Fatal("the read rejected by revocation did not return")
	}
	<-credentials
	setRole(authz.Admin)

	go func() { done <- call("held") }()
	select {
	case <-held:
	case <-ctx.Done():
		t.Fatal("the last approved read did not start")
	}
	if _, err := database.CancelChatExecution(ctx, principal, org, chat.ID, claim.ID); err != nil {
		t.Fatal(err)
	}
	release <- struct{}{}
	select {
	case reply := <-done:
		checkFailure(reply, "observation could not be recorded")
	case <-ctx.Done():
		t.Fatal("the revoked Tool Run did not return")
	}
	<-credentials
	checkFailure(call("stale lease"), "no longer authorized")
	assertNoDispatch()

	queued, err = database.AdmitAgentChat(ctx, principal, org, chat.ID, uuid.New(), conversation.NewMessage{Text: "Resume authorized tools."})
	if err != nil {
		t.Fatal(err)
	}
	claim, err = database.ClaimAgentExecution(ctx, org, queued.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := &agent.AgentState{
		SchemaVersion: agent.StateSchemaVersion,
		SessionID:     claim.SessionID(),
		PermissionCtx: permission.NewContext(permission.ModeBypass),
	}
	if err := database.AgentStateSaver(org, claim).SaveState(ctx, claim.SessionID(), checkpoint); err != nil {
		t.Fatal(err)
	}
	setRole(authz.Viewer)
	exec(`UPDATE agent_execution SET lease_expires_at = clock_timestamp() WHERE org_id = $1 AND execution_id = $2`, org, claim.ID)
	reclaimed, err := database.ClaimAgentExecution(ctx, org, claim.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if restored, writer, err := database.AgentStateSaver(org, reclaimed).Restore(ctx); err != nil ||
		writer != claim.ID || restored == nil || restored.PermissionCtx == nil {
		t.Fatalf("checkpoint could not be restored: %v", err)
	}
	var resumed app.ExecutionProfile
	if err := resumed.UseIntegrationTools(ctx, database, catalog, sealer, org, reclaimed); err == nil {
		t.Fatal("restored execution prepared Tools for a revoked Membership")
	}
	assertNoDispatch()
}
