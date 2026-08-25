package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// JobStatus is where a job has got to. The three terminal values are what the recording
// guard tests, so a job already recorded cannot be recorded a second time.
//
// The values are persisted and appear as literals in the SQL in this package. They are frozen
// by a gate in internal/gates; changing one rewrites what every existing row means.
type JobStatus int16

const (
	JobPending JobStatus = iota
	JobLeased
	JobSucceeded
	JobFailed
	JobCancelled
)

// Job is a unit of work as the control plane holds it.
type Job struct {
	ID              uuid.UUID
	InvestigationID uuid.UUID
	// IntegrationID is what this job reaches: one configured installation. The
	// registration below is where it RUNS. Keeping both is what lets a customer with two
	// clusters behind one Relay have a result attributed to the cluster it was read from
	// rather than to the installation that read it.
	IntegrationID  uuid.UUID
	RegistrationID uuid.UUID
	CapabilityID   string
	// Widened to the protocol's type rather than a plain int, so the version a relay is asked
	// for is the version this holds, with no conversion in between that could narrow it.
	CapabilityVersion uint32
	Arguments         []byte
	LeaseSession      uuid.UUID
	LeaseEpoch        int64
}

// JobFence identifies which execution of a job a message belongs to. Both halves travel on
// the wire — the assignment carries them, the result echoes them — so ownership is validated
// on every job-scoped message rather than inferred from who is currently connected.
type JobFence struct {
	JobID        uuid.UUID
	LeaseSession uuid.UUID
	LeaseEpoch   int64
}

// ErrJobRefused reports work that was not enqueued because its Integration could not carry
// it. Nothing was written.
var ErrJobRefused = errors.New("job refused")

// JobRefusal is why work was not enqueued. Each is a different mistake by whoever planned
// the job, and telling them apart is what makes the boundary diagnosable rather than
// mysterious.
type JobRefusal int

const (
	// JobIntegrationUnknown means no such Integration exists in this organization, or it
	// has been disabled. Both are one answer: an operator who turned an Integration off
	// wants work against it refused, not merely recorded.
	JobIntegrationUnknown JobRefusal = iota + 1
	// JobRelayIsNotTheIntegrations means the job names a Relay that is not the one bound
	// to its Integration — including an Integration no Relay serves. Dispatching anyway
	// would send a read for one customer's cluster to an installation sitting in another.
	JobRelayIsNotTheIntegrations
)

func (r JobRefusal) String() string {
	switch r {
	case JobIntegrationUnknown:
		return "integration unknown or disabled"
	case JobRelayIsNotTheIntegrations:
		return "relay is not the one bound to this integration"
	default:
		return "unrecognised"
	}
}

// EnqueueJob records work to be done. It is pending and unleased: nothing is delivered until
// a session claims it, so a job that is enqueued while every relay is offline waits rather
// than being lost.
//
// The Integration is a PRECONDITION rather than a column copied in. The insert only
// produces a row when a live Integration exists in this organization AND the Relay it is
// bound to is the one the job names, so the tenant boundary is checked on the execution
// path rather than left to whichever query happened to scope itself correctly. Deciding it
// inside the insert also means an Integration disabled between a check and a write cannot
// leave work queued against it.
func (p *Database) EnqueueJob(
	ctx context.Context, organization tenancy.Organization, job Job,
) (JobRefusal, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return 0, err
	}
	tag, err := pool.Exec(ctx, `
		INSERT INTO relay_job
			(job_id, org_id, integration_id, registration_id,
			 capability_id, capability_version, arguments, investigation_id)
		SELECT $1, $2, integration.integration_id, integration.relay_id, $5, $6, $7, $8
		  FROM integration
		 WHERE integration.integration_id = $3
		   AND integration.org_id         = $2
		   AND integration.disabled_at   IS NULL
		   -- The registration is taken FROM the Integration and compared to the one the
		   -- job names, rather than trusted from the job. A caller that got it wrong is
		   -- refused instead of silently redirected.
		   AND integration.relay_id       = $4
		   AND ($8::uuid IS NULL OR EXISTS (
		       SELECT 1 FROM investigation
		        WHERE investigation.org_id = $2
		          AND investigation.investigation_id = $8
		          AND investigation.status = $9
		          FOR NO KEY UPDATE))`,
		job.ID, organization.String(), job.IntegrationID, job.RegistrationID,
		job.CapabilityID, job.CapabilityVersion, job.Arguments,
		nullableUUID(job.InvestigationID), int16(1))
	if err != nil {
		return 0, fmt.Errorf("enqueueing job: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return 0, nil
	}
	return p.explainRefusedJob(ctx, organization, job)
}

// EnqueueVerifiedJob records a provider Tool read only while the Integration's current
// durable verification still grants that exact Relay Capability. The guarded insert closes
// the gap between offering a Tool and dispatching it.
func (p *Database) EnqueueVerifiedJob(
	ctx context.Context, organization tenancy.Organization, job Job,
) error {
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}
	tag, err := pool.Exec(ctx, `
		INSERT INTO relay_job
			(job_id, org_id, integration_id, registration_id,
			 capability_id, capability_version, arguments, investigation_id)
		SELECT $1, $2, integration.integration_id, integration.relay_id, $5, $6, $7, $10
		  FROM integration
		 WHERE integration.integration_id = $3
		   AND integration.org_id = $2
		   AND integration.relay_id = $4
		   AND integration.disabled_at IS NULL
		   AND integration.status IN ($8, $9)
		   AND integration.last_verified_at IS NOT NULL
		   AND coalesce(integration.verify_grants, '[]'::jsonb) @> to_jsonb(ARRAY[$5]::text[])
		   AND ($10::uuid IS NULL OR EXISTS (
		       SELECT 1 FROM investigation
		        WHERE investigation.org_id = $2
		          AND investigation.investigation_id = $10
		          AND investigation.status = $11
		          FOR NO KEY UPDATE))`,
		job.ID, organization.String(), job.IntegrationID, job.RegistrationID,
		job.CapabilityID, job.CapabilityVersion, job.Arguments,
		int16(integrations.StatusActive), int16(integrations.StatusDegraded),
		nullableUUID(job.InvestigationID), int16(1))
	if err != nil {
		return fmt.Errorf("enqueueing verified job: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrJobRefused
	}
	return nil
}

// explainRefusedJob reads why the guarded insert matched nothing.
//
// It is a second read rather than part of the insert, so what it describes is the state a
// moment later. Under a concurrent change the diagnosis can name the wrong reason; what it
// cannot do is be wrong about the refusal itself, because the row was already not written.
// The explanation is for whoever planned the job, not a decision anything acts on.
func (p *Database) explainRefusedJob(
	ctx context.Context, organization tenancy.Organization, job Job,
) (JobRefusal, error) {
	integration, err := p.Integration(ctx, organization, job.IntegrationID)
	switch {
	case errors.Is(err, integrations.ErrUnknown):
		return JobIntegrationUnknown, ErrJobRefused
	case err != nil:
		return 0, fmt.Errorf("auditing a refused job: %w", err)
	case integration.Disabled():
		return JobIntegrationUnknown, ErrJobRefused
	default:
		return JobRelayIsNotTheIntegrations, ErrJobRefused
	}
}
