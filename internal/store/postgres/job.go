package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

type JobStatus int16

const (
	JobPending JobStatus = iota
	JobLeased
	JobSucceeded
	JobFailed
	JobCancelled
)

type RelayJob struct {
	ID                uuid.UUID
	InvestigationID   uuid.UUID
	IntegrationID     uuid.UUID
	RegistrationID    uuid.UUID
	CapabilityID      string
	CapabilityVersion uint32
	Arguments         []byte
	LeaseSession      uuid.UUID
	LeaseEpoch        int64
}

type JobFence struct {
	JobID        uuid.UUID
	LeaseSession uuid.UUID
	LeaseEpoch   int64
}

var ErrJobRefused = errors.New("job refused")

type JobRefusal int

const (
	JobIntegrationUnknown JobRefusal = iota + 1
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

func (p *Database) EnqueueJob(
	ctx context.Context, organization uuid.UUID, job RelayJob) (JobRefusal, error) {
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
		   AND NOT integration.disabled
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
		job.ID, organization, job.IntegrationID, job.RegistrationID,
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

func (p *Database) EnqueueVerifiedJob(
	ctx context.Context, organization uuid.UUID, job RelayJob) error {
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}
	tag, err := pool.Exec(ctx, `
		INSERT INTO relay_job
			(job_id, org_id, integration_id, registration_id,
			 capability_id, capability_version, arguments, investigation_id)
		SELECT $1, $2, integration.integration_id, integration.relay_id, $5, $6, $7, $8
		  FROM integration
		 WHERE integration.integration_id = $3
		   AND integration.org_id = $2
		   AND integration.relay_id = $4
		   AND NOT integration.disabled
		   AND integration.verification_status = 'verified'
		   AND integration.verified_at IS NOT NULL
		   AND $5 = ANY(integration.verification_grants)
		   AND ($8::uuid IS NULL OR EXISTS (
		       SELECT 1 FROM investigation
		        WHERE investigation.org_id = $2
		          AND investigation.investigation_id = $8
		          AND investigation.status = $9
		          FOR NO KEY UPDATE))`,
		job.ID, organization, job.IntegrationID, job.RegistrationID,
		job.CapabilityID, job.CapabilityVersion, job.Arguments,
		nullableUUID(job.InvestigationID), int16(1))
	if err != nil {
		return fmt.Errorf("enqueueing verified job: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrJobRefused
	}
	return nil
}

func (p *Database) explainRefusedJob(
	ctx context.Context, organization uuid.UUID, job RelayJob) (JobRefusal, error) {
	integration, err := p.Integration(ctx, organization, job.IntegrationID)
	switch {
	case errors.Is(err, integrations.ErrUnknown):
		return JobIntegrationUnknown, ErrJobRefused
	case err != nil:
		return 0, fmt.Errorf("auditing a refused job: %w", err)
	case integration.Disabled:
		return JobIntegrationUnknown, ErrJobRefused
	default:
		return JobRelayIsNotTheIntegrations, ErrJobRefused
	}
}
