package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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

func (p *Database) EnqueueVerifiedJob(
	ctx context.Context, organization uuid.UUID, job RelayJob) error {
	pool, err := p.poolForOrganization(organization)
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

type JobCancellation int

const (
	CancellationRefused JobCancellation = iota + 1
	CancellationRecorded
	CancellationRequested
)

func (c JobCancellation) String() string {
	switch c {
	case CancellationRefused:
		return "refused"
	case CancellationRecorded:
		return "recorded"
	case CancellationRequested:
		return "requested"
	default:
		return "unrecognised"
	}
}

func (p *Database) RequestJobCancellation(
	ctx context.Context, organization uuid.UUID, jobID uuid.UUID,
) (JobCancellation, error) {
	pool, err := p.poolForOrganization(organization)
	if err != nil {
		return 0, err
	}

	var resulting JobStatus
	err = pool.QueryRow(ctx, `
		UPDATE relay_job
		   SET status              = CASE WHEN status = 0 THEN 4 ELSE status END,
		       terminal_at         = CASE WHEN status = 0 THEN now() ELSE terminal_at END,
		       -- Coalesced so asking twice does not move the moment it was first asked.
		       cancel_requested_at = COALESCE(cancel_requested_at, now())
		 WHERE job_id       = $1
		   AND org_id = $2
		   AND status IN (0, 1)
		RETURNING status`,
		jobID, organization).Scan(&resulting)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return CancellationRefused, nil
	case err != nil:
		return 0, fmt.Errorf("requesting job cancellation: %w", err)
	case resulting == JobCancelled:
		return CancellationRecorded, nil
	default:
		return CancellationRequested, nil
	}
}

func (p *Database) PendingCancellations(
	ctx context.Context, organization uuid.UUID, sessionID uuid.UUID,
) ([]JobFence, error) {
	pool, err := p.poolForOrganization(organization)
	if err != nil {
		return nil, err
	}

	rows, err := pool.Query(ctx, `
		SELECT job_id, lease_session, lease_epoch
		  FROM relay_job
		 WHERE org_id        = $1
		   AND lease_session       = $2
		   AND status              = 1
		   AND cancel_requested_at IS NOT NULL`,
		organization, sessionID)
	if err != nil {
		return nil, fmt.Errorf("reading pending cancellations: %w", err)
	}
	defer rows.Close()

	var pending []JobFence
	for rows.Next() {
		var fence JobFence
		if err = rows.Scan(&fence.JobID, &fence.LeaseSession, &fence.LeaseEpoch); err != nil {
			return nil, fmt.Errorf("reading a pending cancellation: %w", err)
		}
		pending = append(pending, fence)
	}
	return pending, rows.Err()
}
