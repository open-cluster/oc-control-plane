package storage_test

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	providerslack "github.com/open-cluster/oc-control-plane/internal/integrations/slack"
	"github.com/open-cluster/oc-control-plane/internal/seal"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
	slackwork "github.com/open-cluster/oc-control-plane/internal/webhooks/slack"
)

func TestSlackMessageHeartbeatProtectsSlowProviderLookup(t *testing.T) {
	database, organization := migratedDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	integration, err := connectSlack(t, database, organization, "Slack", slackInstallation("THEARTBEAT"))
	if err != nil {
		t.Fatal(err)
	}
	material := make([]byte, seal.KeyLength)
	if _, err := rand.Read(material); err != nil {
		t.Fatal(err)
	}
	sealer, err := seal.New(material)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := sealer.Seal("test-slack-message-credential", integrations.CredentialBinding(integration.ID))
	if err != nil {
		t.Fatal(err)
	}
	pool, err := database.Pool(organization)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE integration SET credential_sealed = $3
		WHERE org_id = $1 AND integration_id = $2`, organization, integration.ID, sealed); err != nil {
		t.Fatal(err)
	}
	outcome, err := database.RecordSlackMessage(ctx, organization, storage.SlackMessage{
		Integration: integration.ID, ContentDigest: randomDigest(t),
		Channel: "CHEARTBEAT", Thread: "1700000001.000001", MessageID: "1700000001.000001",
		Subject: "slow provider", ActorID: "UWORK", Text: "why?",
	})
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"url":"https://test.slack.com/","user_id":"UWORK","team_id":"THEARTBEAT"}`)
	}))
	t.Cleanup(vendor.Close)
	var released sync.Once
	unblock := func() { released.Do(func() { close(release) }) }
	defer unblock()
	worker := slackwork.MessageWorker{
		Database: database, Owner: "slow-lookup-worker", Lease: 600 * time.Millisecond, WindowLead: time.Hour,
		References: &slackwork.SlackReferenceResolver{
			Database: database, Client: providerslack.NewClient(vendor.URL), Sealer: sealer,
		},
	}
	finished := make(chan error, 1)
	go func() {
		worked, err := worker.ProcessOne(ctx)
		if err == nil && !worked {
			err = errors.New("accepted message was not claimed")
		}
		finished <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("accepted message never reached the provider")
	}
	select {
	case <-time.After(900 * time.Millisecond):
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if work, found, err := database.ClaimSlackMessageWork(ctx, "replacement-worker", time.Minute); err != nil || found {
		t.Fatalf("slow lookup lost its renewed lease: %+v, %v, %v", work, found, err)
	}
	unblock()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	detail, err := database.ConversationDetail(ctx, organization, outcome.Conversation, 50)
	if err != nil || len(detail.Turns) != 1 || len(detail.Messages) != 1 ||
		detail.Messages[0].SourceReference != "https://test.slack.com/archives/CHEARTBEAT/p1700000001000001" {
		t.Fatalf("slow lookup lost the accepted turn or source reference: %+v, %v", detail, err)
	}
}
