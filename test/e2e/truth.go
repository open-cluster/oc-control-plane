package e2e

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// jobStatus mirrors the durable status column. It is redeclared here rather than imported
// because the schema is the contract this harness holds an implementation to — importing the
// implementation's own constants would make the harness agree with it by construction.
type jobStatus int16

const (
	jobPending jobStatus = iota
	jobLeased
	jobSucceeded
	jobFailed
	jobCancelled
)

func (s jobStatus) String() string {
	switch s {
	case jobPending:
		return "pending"
	case jobLeased:
		return "leased"
	case jobSucceeded:
		return "succeeded"
	case jobFailed:
		return "failed"
	case jobCancelled:
		return "cancelled"
	default:
		return fmt.Sprintf("unrecognised(%d)", int16(s))
	}
}

func (s jobStatus) terminal() bool {
	return s == jobSucceeded || s == jobFailed || s == jobCancelled
}

const databaseStartTimeout = 3 * time.Minute

// truth is the durable state both halves are measured against.
//
// The harness reads and writes it directly, in SQL, rather than through either
// implementation's helpers or through protocol messages. Messages would prove they were sent;
// the guarantee is about what was written.
type truth struct {
	container *tcpostgres.PostgresContainer
	pool      *pgxpool.Pool
	dsn       string
}

// startTruth brings up the database the control plane will be given.
func startTruth(ctx context.Context) (*truth, error) {
	startCtx, cancel := context.WithTimeout(ctx, databaseStartTimeout)
	defer cancel()

	container, err := tcpostgres.Run(startCtx, "postgres:17-alpine",
		tcpostgres.WithDatabase("controlplane"),
		tcpostgres.WithUsername("controlplane"),
		tcpostgres.WithPassword("controlplane"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(2*time.Minute)),
	)
	if err != nil {
		return nil, fmt.Errorf("starting postgres: %w", err)
	}

	dsn, err := container.ConnectionString(startCtx, "sslmode=disable")
	if err != nil {
		_ = testcontainers.TerminateContainer(container)
		return nil, fmt.Errorf("reading the connection string: %w", err)
	}
	return &truth{container: container, dsn: dsn}, nil
}

// connect opens the harness's own pool and reaches the schema through it. It is deferred
// until after the control plane has started, because the control plane applies the migrations
// and there is nothing to read before it has.
//
// The read at the end is the point. Building a pool dials nothing — it would succeed against
// a database that is gone — and the first thing to notice would otherwise be a test failing
// on an assertion about a job.
func (t *truth) connect(ctx context.Context) error {
	pool, err := pgxpool.New(ctx, t.dsn)
	if err != nil {
		return fmt.Errorf("connecting to the database: %w", err)
	}
	t.pool = pool

	if _, err = pool.Exec(ctx, `SELECT 1 FROM relay_job WHERE false`); err != nil {
		pool.Close()
		t.pool = nil
		return fmt.Errorf("reading the schema the control plane migrated: %w", err)
	}
	return nil
}

func (t *truth) close() {
	if t == nil {
		return
	}
	if t.pool != nil {
		t.pool.Close()
	}
	_ = testcontainers.TerminateContainer(t.container)
}

// issueBootstrapToken mints a single-use enrolment token and records its digest, which is
// what an operator issuing one would leave behind. The token itself is returned once and
// held nowhere else, exactly as the real issuance path requires.
func (t *truth) issueBootstrapToken(ctx context.Context, organization string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generating a bootstrap token: %w", err)
	}
	token := hex.EncodeToString(raw)
	digest := sha256.Sum256([]byte(token))

	_, err := t.pool.Exec(ctx, `
		INSERT INTO relay_bootstrap_token (token_digest, organization, expires_at)
		VALUES ($1, $2, $3)`,
		digest[:], organization, time.Now().Add(time.Hour))
	if err != nil {
		return "", fmt.Errorf("issuing a bootstrap token: %w", err)
	}
	return token, nil
}

// registration reports the identity an organization's relay enrolled with, and whether one
// exists yet. Absence is not an error: the caller is usually waiting for enrolment.
func (t *truth) registration(ctx context.Context, organization string) (uuid.UUID, bool, error) {
	var id uuid.UUID
	err := t.pool.QueryRow(ctx, `
		SELECT registration_id
		  FROM relay_registration
		 WHERE organization = $1
		 ORDER BY created_at DESC
		 LIMIT 1`, organization).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("reading the relay registration: %w", err)
	}
	return id, true, nil
}

// countRegistrations reports how many identities an organization has. It is how a spent
// token is proven not to have minted a second one.
func (t *truth) countRegistrations(ctx context.Context, organization string) (int, error) {
	var count int
	err := t.pool.QueryRow(ctx,
		`SELECT count(*) FROM relay_registration WHERE organization = $1`,
		organization).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("counting relay registrations: %w", err)
	}
	return count, nil
}

// evidenceConnection records the Kubernetes Connection every job reaches, in the
// organization's Default Environment and served by the enrolled relay.
//
// The Connection is what a job reaches; the relay is where it runs. Both rows are written here
// in SQL for the same reason everything else in this harness is: the schema is the contract an
// implementation is being held to, and going through the implementation's own helpers would
// make the harness agree with it by construction.
func (t *truth) evidenceConnection(
	ctx context.Context, organization string, registration uuid.UUID,
) (uuid.UUID, error) {
	environment := uuid.New()
	if _, err := t.pool.Exec(ctx, `
		INSERT INTO environment (environment_id, organization, name, is_default)
		VALUES ($1, $2, 'Default', TRUE)
		ON CONFLICT (organization) WHERE is_default DO NOTHING`,
		environment, organization); err != nil {
		return uuid.Nil, fmt.Errorf("creating the default environment: %w", err)
	}
	if err := t.pool.QueryRow(ctx,
		`SELECT environment_id FROM environment WHERE organization = $1 AND is_default`,
		organization).Scan(&environment); err != nil {
		return uuid.Nil, fmt.Errorf("reading the default environment: %w", err)
	}

	connection := uuid.New()
	if _, err := t.pool.Exec(ctx, `
		INSERT INTO connection
			(connection_id, organization, environment_id, integration, name,
			 role, locality, relay_registration_id)
		VALUES ($1, $2, $3, 'kubernetes', $4, 2, 2, $5)`,
		connection, organization, environment, "the cluster "+connection.String(),
		registration); err != nil {
		return uuid.Nil, fmt.Errorf("creating the evidence connection: %w", err)
	}
	return connection, nil
}

// enqueueJob records work to be done, pending and unleased — which is what an investigation
// planner will do once one exists. Nothing is delivered until a session claims it, so a job
// enqueued while every relay is offline waits rather than being lost.
//
// The Connection is named rather than left out, because the schema will not accept a job
// without one: the environment boundary is a checked precondition on the execution path, and a
// job with nothing to compare against could not be one.
func (t *truth) enqueueJob(
	ctx context.Context, organization string, registration, connection uuid.UUID,
	capability string, version uint32, arguments []byte,
) (uuid.UUID, error) {
	id := uuid.New()
	_, err := t.pool.Exec(ctx, `
		INSERT INTO relay_job
			(job_id, organization, connection_id, registration_id,
			 capability_id, capability_version, arguments)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		id, organization, connection, registration, capability, version, arguments)
	if err != nil {
		return uuid.Nil, fmt.Errorf("enqueueing a job: %w", err)
	}
	return id, nil
}

// jobRecord is a job as the database holds it.
type jobRecord struct {
	Status     jobStatus
	Result     []byte
	LeaseEpoch int64
}

func (t *truth) job(ctx context.Context, organization string, id uuid.UUID) (jobRecord, error) {
	var record jobRecord
	err := t.pool.QueryRow(ctx, `
		SELECT status, result, lease_epoch
		  FROM relay_job
		 WHERE job_id = $1 AND organization = $2`, id, organization).
		Scan(&record.Status, &record.Result, &record.LeaseEpoch)
	if err != nil {
		return jobRecord{}, fmt.Errorf("reading job %s: %w", id, err)
	}
	return record, nil
}

// occurrencesOf searches EVERY text and binary column of every table for a string, and reports
// where it was found.
//
// This is deliberately a sweep rather than a read of the one column a secret was expected in. The
// claim being tested is that a secret does not reach the control plane's durable state AT ALL,
// and a test that checked only the column somebody thought of would pass against an
// implementation that also wrote it to an audit row, a log table or a cached projection. The
// schema is read from the catalogue rather than listed here, so a table added later is swept
// without anyone remembering to add it.
func (t *truth) occurrencesOf(ctx context.Context, needle string) ([]string, error) {
	rows, err := t.pool.Query(ctx, `
		SELECT table_name, column_name, data_type
		  FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND data_type IN ('text', 'character varying', 'character', 'json', 'jsonb', 'bytea')
		 ORDER BY table_name, column_name`)
	if err != nil {
		return nil, fmt.Errorf("reading the schema to sweep: %w", err)
	}

	type column struct{ table, name, kind string }
	var columns []column
	for rows.Next() {
		var found column
		if err = rows.Scan(&found.table, &found.name, &found.kind); err != nil {
			rows.Close()
			return nil, fmt.Errorf("reading the schema to sweep: %w", err)
		}
		columns = append(columns, found)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the schema to sweep: %w", err)
	}
	// A sweep that found no columns would report success while having looked at nothing, which is
	// exactly the failure mode a negative assertion has to be built against.
	if len(columns) == 0 {
		return nil, errors.New("no text or binary columns found; the sweep would be vacuous")
	}

	var found []string
	for _, candidate := range columns {
		// bytea is cast rather than skipped: a serialized protocol message is stored as bytes, and
		// it is the single most likely place for an unredacted value to survive.
		expression := fmt.Sprintf("%q::text", candidate.name)
		if candidate.kind == "bytea" {
			expression = fmt.Sprintf("encode(%q, 'escape')", candidate.name)
		}
		var count int64
		query := fmt.Sprintf(
			`SELECT count(*) FROM %q WHERE %s LIKE '%%' || $1 || '%%'`, candidate.table, expression)
		if err = t.pool.QueryRow(ctx, query, needle).Scan(&count); err != nil {
			return nil, fmt.Errorf("sweeping %s.%s: %w", candidate.table, candidate.name, err)
		}
		if count > 0 {
			found = append(found, fmt.Sprintf("%s.%s (%d row(s))",
				candidate.table, candidate.name, count))
		}
	}
	return found, nil
}
