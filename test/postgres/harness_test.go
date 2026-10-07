package storage_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/conversation"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	storage "github.com/open-cluster/oc-control-plane/internal/store/postgres"
	slackwork "github.com/open-cluster/oc-control-plane/internal/webhooks/slack"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

func postgresDSN(t *testing.T) string {
	t.Helper()
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
		noContainerRuntime(t, err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(container) })

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	return dsn
}

func openDatabaseForTest(t *testing.T, dsn string) *storage.Database {
	t.Helper()
	opened, err := storage.OpenDatabase(context.Background(), dsn)
	if err != nil {
		t.Fatalf("OpenDatabase: %v", err)
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		opened.Close()
		t.Fatalf("opening test-owned database pool: %v", err)
	}
	testDatabasePools.Store(opened, pool)
	t.Cleanup(func() {
		testDatabasePools.Delete(opened)
		pool.Close()
		opened.Close()
	})
	return opened
}

var testDatabasePools sync.Map

func poolForTest(database *storage.Database, organization uuid.UUID) (*pgxpool.Pool, error) {
	if organization == uuid.Nil {
		return nil, storage.ErrUnknownOrganization
	}
	pool, ok := testDatabasePools.Load(database)
	if !ok {
		return nil, errors.New("test-owned database pool is unavailable")
	}
	return pool.(*pgxpool.Pool), nil
}

func appendMessageForTest(
	database *storage.Database, ctx context.Context, _ authz.Principal, organization uuid.UUID,
	id uuid.UUID, said conversation.NewMessage,
) (conversation.Message, error) {
	pool, err := poolForTest(database, organization)
	if err != nil {
		return conversation.Message{}, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return conversation.Message{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var state conversation.State
	if err = tx.QueryRow(ctx, `SELECT state FROM conversation
		WHERE org_id = $1 AND conversation_id = $2 FOR UPDATE`, organization, id).Scan(&state); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return conversation.Message{}, conversation.ErrUnknown
		}
		return conversation.Message{}, err
	}
	if state != conversation.StateOpen {
		return conversation.Message{}, conversation.ErrClosed
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	window := conversation.Window{From: now.Add(-conversation.DefaultIncidentWindowLead), Until: now}
	if said.Window != nil {
		window = said.Window.Normalized()
	}
	var written conversation.Message
	var role conversation.Role
	var actorKind conversation.ActorKind
	var investigationID *uuid.UUID
	if err = tx.QueryRow(ctx, `INSERT INTO conversation_message
		(conversation_id, org_id, sequence, role, actor_kind, actor_id, actor_display, text, window_from, window_until)
		SELECT $1, $2, coalesce((SELECT max(sequence) FROM conversation_message
		 WHERE org_id = $2 AND conversation_id = $1), 0) + 1, $3, $4, $5, $6, $7, $8, $9
		RETURNING sequence, role, actor_kind, actor_id, actor_display, text, source_reference,
		          investigation_id, created_at`, id, organization, int16(said.Role), int16(said.ActorKind),
		said.ActorID, said.ActorDisplay, said.Text, window.From, window.Until).Scan(
		&written.Sequence, &role, &actorKind, &written.ActorID, &written.ActorDisplay,
		&written.Text, &written.SourceReference, &investigationID, &written.CreatedAt); err != nil {
		return conversation.Message{}, err
	}
	written.Role, written.ActorKind = role, actorKind
	if investigationID != nil {
		written.InvestigationID = *investigationID
	}
	written.WindowFrom, written.WindowUntil = window.From, window.Until
	if _, err = tx.Exec(ctx, `UPDATE conversation SET last_activity_at = now()
		WHERE org_id = $1 AND conversation_id = $2`, organization, id); err != nil {
		return conversation.Message{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return conversation.Message{}, err
	}
	return written, nil
}

func openTurnForTest(
	database *storage.Database, ctx context.Context, organization, conversationID uuid.UUID,
	lead time.Duration,
) (conversation.Turn, bool, error) {
	opened, err := database.DrainConversation(ctx, organization, conversationID, lead, 1_000_000)
	if err != nil || !opened {
		return conversation.Turn{}, opened, err
	}
	pool, err := poolForTest(database, organization)
	if err != nil {
		return conversation.Turn{}, false, err
	}
	var turn conversation.Turn
	var status investigation.Status
	err = pool.QueryRow(ctx, `SELECT investigation_id, turn, status, created_at
		FROM investigation WHERE org_id = $1 AND conversation_id = $2
		ORDER BY turn DESC LIMIT 1`, organization, conversationID).Scan(
		&turn.InvestigationID, &turn.Ordinal, &status, &turn.CreatedAt)
	turn.Status = status.String()
	return turn, err == nil, err
}

func waitingTurnsForTest(
	database *storage.Database, ctx context.Context, organization uuid.UUID,
) (int, error) {
	pool, err := poolForTest(database, organization)
	if err != nil {
		return 0, err
	}
	var waiting int
	err = pool.QueryRow(ctx, `SELECT count(*) FROM investigation
		WHERE org_id = $1 AND status = 1 AND lease_worker = ''`, organization).Scan(&waiting)
	return waiting, err
}

func slackReplyStateForTest(
	database *storage.Database, ctx context.Context, organization, investigationID uuid.UUID,
) (status int, sequence int64, streamTS, note string, found bool, err error) {
	pool, err := poolForTest(database, organization)
	if err != nil {
		return 0, 0, "", "", false, err
	}
	err = pool.QueryRow(ctx, `SELECT status, last_sequence, stream_ts, note FROM slack_reply
		WHERE investigation_id = $1 AND org_id = $2`, investigationID, organization).Scan(
		&status, &sequence, &streamTS, &note)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, "", "", false, nil
	}
	return status, sequence, streamTS, note, err == nil, err
}

func issueBootstrapTokenForTest(
	database *storage.Database, ctx context.Context, organization uuid.UUID,
	digest []byte, expiresAt time.Time,
) error {
	pool, err := poolForTest(database, organization)
	if err != nil {
		return err
	}
	_, err = pool.Exec(ctx, `INSERT INTO relay_bootstrap_token (bootstrap_digest, org_id, expires_at)
		VALUES ($1, $2, $3)`, digest, organization, expiresAt)
	return err
}

func organization(t *testing.T, id string) uuid.UUID {
	t.Helper()
	if _, err := uuid.Parse(id); err != nil {
		id = uuid.NewSHA1(uuid.NameSpaceOID, []byte(id)).String()
	}
	value, err := uuid.Parse(id)
	if err != nil {
		t.Fatalf("NewOrganization(%q): %v", id, err)
	}
	return value
}

func ownerOf(t *testing.T, organization uuid.UUID) authz.Principal {
	t.Helper()
	return memberOf(t, organization, authz.Admin)
}

func memberOf(
	t *testing.T, organization uuid.UUID, role authz.Role,
) authz.Principal {
	t.Helper()

	principal, err := authz.NewPrincipal(uuid.New(), uuid.New(), "Test Operator", "",
		authz.Membership{Organization: organization, Role: role})
	if err != nil {
		t.Fatalf("building a principal: %v", err)
	}
	return principal
}

func sessionPrincipal(
	t *testing.T, database *storage.Database, digest []byte, user uuid.UUID,
) authz.Principal {
	t.Helper()
	signedIn, err := database.SessionByToken(context.Background(), digest)
	if err != nil {
		t.Fatalf("resolving session principal: %v", err)
	}
	principal, err := authz.NewPrincipal(user, signedIn.Session.ID, "Admin", "admin@example.test", signedIn.Membership)
	if err != nil {
		t.Fatalf("building session principal: %v", err)
	}
	return principal
}

const requireContainers = "OC_REQUIRE_CONTAINERS"

func noContainerRuntime(t *testing.T, err error) {
	t.Helper()
	if os.Getenv(requireContainers) != "" {
		t.Fatalf("no container runtime is reachable and %s is set, so this suite must not "+
			"report success without having run: %v", requireContainers, err)
	}
	t.Skipf("cannot start postgres (is the Docker daemon reachable?): %v", err)
}

const runtimeProbe = "OC_CONTAINER_RUNTIME_PROBE"

func TestNoContainerRuntimeProbe(t *testing.T) {
	if os.Getenv(runtimeProbe) != "1" {
		t.Skip("probe: run by TestAMissingRuntime… through a subprocess")
	}
	noContainerRuntime(t, errors.New("injected: no runtime"))
}

func TestAMissingRuntimeSkipsWhenNoneWasPromised(t *testing.T) {
	t.Parallel()

	output, err := runProbe(t, "")
	if err != nil {
		t.Fatalf("the probe failed where it should have skipped: %v\n%s", err, output)
	}
	if !strings.Contains(output, "cannot start postgres") {
		t.Errorf("a skipped probe does not say why:\n%s", output)
	}
}

func TestAMissingRuntimeFailsWhenContainersWereRequired(t *testing.T) {
	t.Parallel()

	output, err := runProbe(t, "1")
	if err == nil {
		t.Fatalf("the probe passed with %s set; a suite that could not run reported "+
			"success, which is the whole defect:\n%s", requireContainers, output)
	}
	if !strings.Contains(output, "must not report success without having run") {
		t.Errorf("the failure does not say what went wrong:\n%s", output)
	}
}

func runProbe(t *testing.T, required string) (string, error) {
	t.Helper()

	command := exec.Command(os.Args[0], "-test.run", "^TestNoContainerRuntimeProbe$", "-test.v")
	command.Env = append(os.Environ(), runtimeProbe+"=1", requireContainers+"="+required)
	output, err := command.CombinedOutput()
	return string(output), err
}

func claimToken(t *testing.T, database *storage.Database, org uuid.UUID, id uuid.UUID) uuid.UUID {
	t.Helper()
	pool, err := poolForTest(database, org)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err = pool.Exec(ctx, `UPDATE investigation
		SET lease_worker = 'fixture', lease_token = gen_random_uuid(), lease_expires_at = now() + interval '15 minutes'
		WHERE org_id = $1 AND investigation_id = $2 AND status = 1 AND lease_worker = ''`, org, id); err != nil {
		t.Fatal(err)
	}
	var value *uuid.UUID
	if err = pool.QueryRow(ctx, `SELECT lease_token FROM investigation WHERE org_id = $1 AND investigation_id = $2`, org, id).Scan(&value); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil
		}
		t.Fatal(err)
	}
	if value == nil {
		return uuid.Nil
	}
	return *value
}

type slackMessageFixture struct {
	database     *storage.Database
	organization uuid.UUID
	other        uuid.UUID
	integration  uuid.UUID
	conversation uuid.UUID
	delivery     uuid.UUID
	worker       slackwork.MessageWorker
}

type storedSlackMessageWork struct {
	Status       storage.SlackMessageWorkStatus
	Attempts     int
	FailureClass string
	AvailableAt  time.Time
	UpdatedAt    time.Time
}

func readSlackMessageWork(t *testing.T, fixture slackMessageFixture) storedSlackMessageWork {
	t.Helper()
	pool, err := poolForTest(fixture.database, fixture.organization)
	if err != nil {
		t.Fatal(err)
	}
	var work storedSlackMessageWork
	if err := pool.QueryRow(context.Background(), `SELECT status, attempts, failure_class, available_at, updated_at
		FROM slack_message_work WHERE org_id = $1 AND conversation_id = $2 AND message_sequence = 1`,
		fixture.organization, fixture.conversation).Scan(&work.Status, &work.Attempts, &work.FailureClass,
		&work.AvailableAt, &work.UpdatedAt); err != nil {
		t.Fatal(err)
	}
	return work
}

func acceptedSlackMessage(t *testing.T) slackMessageFixture {
	t.Helper()
	database, organization, other := twoOrganizationsInOneDatabase(t)
	ctx := context.Background()
	integration, err := connectSlack(t, database, organization, "Slack", slackInstallation("TWORK"))
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := database.RecordSlackMessage(ctx, organization, storage.SlackMessage{
		Integration: integration.ID, ContentDigest: randomDigest(t),
		Channel: "CWORK", Thread: "1.0", Subject: "accepted question", ActorID: "UWORK", Text: "why?",
	})
	if err != nil {
		t.Fatal(err)
	}
	var delivery uuid.UUID
	pool, err := poolForTest(database, organization)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT delivery_id FROM slack_message_work
		WHERE org_id = $1 AND conversation_id = $2 AND message_sequence = 1`,
		organization, outcome.Conversation).Scan(&delivery); err != nil {
		t.Fatal(err)
	}
	return slackMessageFixture{
		database: database, organization: organization, other: other, integration: integration.ID,
		conversation: outcome.Conversation, delivery: delivery,
		worker: slackwork.MessageWorker{
			Database: database, Owner: "slack-message-worker", WindowLead: 2 * time.Hour,
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		},
	}
}
