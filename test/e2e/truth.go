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

type truth struct {
	container *tcpostgres.PostgresContainer
	pool      *pgxpool.Pool
	dsn       string
}

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

func (t *truth) issueBootstrapToken(ctx context.Context, organization string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generating a bootstrap token: %w", err)
	}
	token := hex.EncodeToString(raw)
	digest := sha256.Sum256([]byte(token))

	_, err := t.pool.Exec(ctx, `
		INSERT INTO relay_bootstrap_token (bootstrap_digest, org_id, expires_at)
		VALUES ($1, $2, $3)`,
		digest[:], organization, time.Now().Add(time.Hour))
	if err != nil {
		return "", fmt.Errorf("issuing a bootstrap token: %w", err)
	}
	return token, nil
}

func (t *truth) registration(ctx context.Context, organization string) (uuid.UUID, bool, error) {
	var id uuid.UUID
	err := t.pool.QueryRow(ctx, `
		SELECT registration_id
		  FROM relay_registration
		 WHERE org_id = $1
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

func (t *truth) countRegistrations(ctx context.Context, organization string) (int, error) {
	var count int
	err := t.pool.QueryRow(ctx,
		`SELECT count(*) FROM relay_registration WHERE org_id = $1`,
		organization).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("counting relay registrations: %w", err)
	}
	return count, nil
}

func (t *truth) kubernetesIntegration(
	ctx context.Context, organization string, registration uuid.UUID,
) (uuid.UUID, error) {
	integration := uuid.New()
	if _, err := t.pool.Exec(ctx, `
		INSERT INTO integration
			(integration_id, org_id, provider, name, relay_id)
		VALUES ($1, $2, 'kubernetes', $3, $4)`,
		integration, organization, "the cluster "+integration.String(),
		registration); err != nil {
		return uuid.Nil, fmt.Errorf("creating the kubernetes integration: %w", err)
	}
	return integration, nil
}

func (t *truth) enqueueJob(
	ctx context.Context, organization string, registration, integration uuid.UUID,
	capability string, version uint32, arguments []byte,
) (uuid.UUID, error) {
	id := uuid.New()
	_, err := t.pool.Exec(ctx, `
		INSERT INTO relay_job
			(job_id, org_id, integration_id, registration_id,
			 capability_id, capability_version, arguments)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		id, organization, integration, registration, capability, version, arguments)
	if err != nil {
		return uuid.Nil, fmt.Errorf("enqueueing a job: %w", err)
	}
	return id, nil
}

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
		 WHERE job_id = $1 AND org_id = $2`, id, organization).
		Scan(&record.Status, &record.Result, &record.LeaseEpoch)
	if err != nil {
		return jobRecord{}, fmt.Errorf("reading job %s: %w", id, err)
	}
	return record, nil
}

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
	if len(columns) == 0 {
		return nil, errors.New("no text or binary columns found; the sweep would be vacuous")
	}

	var found []string
	for _, candidate := range columns {
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
