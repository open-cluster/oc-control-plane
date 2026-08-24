package storage

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/open-cluster/oc-control-plane/internal/changeledger"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

func TestTenantOwnedHelpersPredicateOnOrganization(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: requires a Docker daemon")
	}
	ctx := context.Background()
	container, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("controlplane"),
		tcpostgres.WithUsername("controlplane"),
		tcpostgres.WithPassword("controlplane"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(2*time.Minute)),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(container) })
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("postgres connection string: %v", err)
	}
	database, err := OpenDatabase(ctx, dsn)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(database.Close)
	if _, err = database.Migrate(ctx); err != nil {
		t.Fatalf("migrate database: %v", err)
	}

	first := mustOrganization(t, "org-first")
	second := mustOrganization(t, "org-second")
	integrationID := uuid.New()
	conversationID := uuid.New()
	episodeID := uuid.New()
	signalID := uuid.New()
	observedAt := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	seed := &pgx.Batch{}
	seed.Queue(`
		INSERT INTO integration (integration_id, org_id, integration_type_id, name)
		VALUES ($1, $2, 1, 'tenant predicate test')`, integrationID, first.String())
	seed.Queue(`
		INSERT INTO conversation
			(conversation_id, org_id, surface, subject, created_by)
		VALUES ($1, $2, 2, 'tenant predicate test', 'test')`, conversationID, first.String())
	seed.Queue(`
		INSERT INTO slack_conversation
			(conversation_id, org_id, integration_id, channel_id, thread_ts)
		VALUES ($1, $2, $3, 'C-BOUNDARY', '1700000000.1')`,
		conversationID, first.String(), integrationID)
	seed.Queue(`
		INSERT INTO incident_episode
			(episode_id, org_id, integration_id, grouping_key, grouping_basis,
			 title, status, first_seen_at, last_seen_at, signal_count)
		VALUES ($1, $2, $3, 'boundary', 1, 'tenant predicate test', 1, $4, $4, 0)`,
		episodeID, first.String(), integrationID, observedAt)
	seed.Queue(`
		INSERT INTO signal
			(signal_id, org_id, integration_id, source_key, status, title, summary,
			 started_at, episode_id)
		VALUES ($1, $2, $3, 'boundary-signal', 1, 'tenant predicate test', '', $4, $5)`,
		signalID, first.String(), integrationID, observedAt, episodeID)
	seed.Queue(`
		INSERT INTO change_ledger_scope
			(integration_id, org_id, requested_interval_seconds)
		VALUES ($1, $2, 60)`, integrationID, first.String())
	if err = database.pool.SendBatch(ctx, seed).Close(); err != nil {
		t.Fatalf("seed tenant-owned rows: %v", err)
	}

	t.Run("Slack thread lookup", func(t *testing.T) {
		transaction, beginErr := database.pool.Begin(ctx)
		if beginErr != nil {
			t.Fatal(beginErr)
		}
		defer func() { _ = transaction.Rollback(ctx) }()
		got, _, bindErr := bindThread(ctx, transaction, second, SlackMessage{
			Integration: integrationID,
			Channel:     "C-BOUNDARY",
			Thread:      "1700000000.1",
			Subject:     "must remain isolated",
			ActorID:     "U-SECOND",
		})
		if bindErr == nil && got == conversationID {
			t.Fatal("a Slack lookup returned another Organization's Conversation")
		}
	})

	t.Run("episode refresh", func(t *testing.T) {
		transaction, beginErr := database.pool.Begin(ctx)
		if beginErr != nil {
			t.Fatal(beginErr)
		}
		if refreshErr := refreshEpisode(ctx, transaction, second, episodeID); refreshErr != nil {
			_ = transaction.Rollback(ctx)
			t.Fatal(refreshErr)
		}
		if commitErr := transaction.Commit(ctx); commitErr != nil {
			t.Fatal(commitErr)
		}
		var count int
		if queryErr := database.pool.QueryRow(ctx,
			`SELECT signal_count FROM incident_episode WHERE episode_id = $1`, episodeID).
			Scan(&count); queryErr != nil {
			t.Fatal(queryErr)
		}
		if count != 0 {
			t.Fatalf("another Organization refreshed the episode to %d signals", count)
		}
	})

	t.Run("ledger scope advancement", func(t *testing.T) {
		transaction, beginErr := database.pool.Begin(ctx)
		if beginErr != nil {
			t.Fatal(beginErr)
		}
		if advanceErr := advanceChangeLedgerScope(ctx, transaction, second,
			changeledger.Delta{IntegrationID: integrationID, ObservedAt: observedAt}, 0,
		); advanceErr != nil {
			_ = transaction.Rollback(ctx)
			t.Fatal(advanceErr)
		}
		if commitErr := transaction.Commit(ctx); commitErr != nil {
			t.Fatal(commitErr)
		}
		var confirmed *time.Time
		if queryErr := database.pool.QueryRow(ctx,
			`SELECT last_confirmed_at FROM change_ledger_scope WHERE integration_id = $1`,
			integrationID).Scan(&confirmed); queryErr != nil {
			t.Fatal(queryErr)
		}
		if confirmed != nil {
			t.Fatalf("another Organization advanced the ledger scope to %v", *confirmed)
		}
	})
}

func mustOrganization(t *testing.T, value string) tenancy.Organization {
	t.Helper()
	organization, err := tenancy.NewOrganization(value)
	if err != nil {
		t.Fatalf("organization %q: %v", value, err)
	}
	return organization
}
