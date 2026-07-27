package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// JobStatus is where a job has got to. The three terminal values are what the recording
// guard tests, so a job already recorded cannot be recorded a second time.
type JobStatus int16

const (
	JobPending JobStatus = iota
	JobLeased
	JobSucceeded
	JobFailed
	JobCancelled
)

// ErrResultRefused reports that a result was not recorded because the job was not in a state
// to accept it. The reason distinguishes a benign race from something worth alarming about,
// but either way nothing was written.
var ErrResultRefused = errors.New("job result refused")

// ResultRefusal is why a result was not recorded.
type ResultRefusal int

const (
	// ResultAlreadyRecorded means the job reached a terminal state before this result
	// arrived. It is the expected answer to a relay resending a result it never saw
	// acknowledged, so it is reported as a definitive outcome rather than an error: the
	// relay must stop resending, and its buffer must drain.
	ResultAlreadyRecorded ResultRefusal = iota + 1
	// ResultFenceSuperseded means the result echoed a lease that no longer owns the job —
	// an older session, or an older generation of this one. Recording it would overwrite the
	// outcome of the execution that currently owns the work.
	ResultFenceSuperseded
	// ResultJobUnknown means no such job exists for this organization.
	ResultJobUnknown
)

func (r ResultRefusal) String() string {
	switch r {
	case ResultAlreadyRecorded:
		return "already recorded"
	case ResultFenceSuperseded:
		return "lease superseded"
	case ResultJobUnknown:
		return "job unknown"
	default:
		return "unrecognised"
	}
}

// Job is a unit of work as the control plane holds it.
type Job struct {
	ID                uuid.UUID
	RegistrationID    uuid.UUID
	CapabilityID      string
	CapabilityVersion int
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

// EnqueueJob records work to be done. It is pending and unleased: nothing is delivered until
// a session claims it, so a job that is enqueued while every relay is offline waits rather
// than being lost.
func (p *Placements) EnqueueJob(ctx context.Context, organization tenancy.Organization, job Job) error {
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO relay_job
			(job_id, organization, registration_id, capability_id, capability_version, arguments)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		job.ID, organization.String(), job.RegistrationID,
		job.CapabilityID, job.CapabilityVersion, job.Arguments)
	if err != nil {
		return fmt.Errorf("enqueueing job: %w", err)
	}
	return nil
}

// ClaimJobs leases up to limit jobs for a session and returns them. The transition to leased
// commits before anything is delivered: a crash between claiming and sending leaves a leased
// job whose lease expires and is swept, which is recoverable. Sending first and claiming
// after would leave work delivered but unrecorded, which is not.
//
// Expired leases are claimable again, and every claim raises the generation, so a result
// from the execution that lost its lease is refused rather than recorded.
func (p *Placements) ClaimJobs(
	ctx context.Context,
	organization tenancy.Organization,
	claim JobClaim,
) ([]Job, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return nil, err
	}

	rows, err := pool.Query(ctx, `
		UPDATE relay_job
		   SET status           = 1,
		       lease_session    = $3,
		       lease_epoch      = lease_epoch + 1,
		       lease_expires_at = now() + $4::interval
		 WHERE job_id IN (
		       SELECT job_id
		         FROM relay_job
		        WHERE organization    = $1
		          AND registration_id = $2
		          AND (status = 0 OR (status = 1 AND lease_expires_at <= now()))
		        ORDER BY created_at
		        LIMIT $5
		        -- Concurrent claimers take disjoint work instead of blocking on each other.
		        FOR UPDATE SKIP LOCKED)
		RETURNING job_id, registration_id, capability_id, capability_version,
		          arguments, lease_session, lease_epoch`,
		organization.String(), claim.RegistrationID, claim.SessionID,
		claim.LeaseFor.String(), claim.Limit)
	if err != nil {
		return nil, fmt.Errorf("claiming jobs: %w", err)
	}
	defer rows.Close()

	var claimed []Job
	for rows.Next() {
		var job Job
		if err = rows.Scan(&job.ID, &job.RegistrationID, &job.CapabilityID,
			&job.CapabilityVersion, &job.Arguments, &job.LeaseSession, &job.LeaseEpoch); err != nil {
			return nil, fmt.Errorf("reading claimed job: %w", err)
		}
		claimed = append(claimed, job)
	}
	return claimed, rows.Err()
}

// JobClaim is what a session asks for when it takes work.
type JobClaim struct {
	RegistrationID uuid.UUID
	SessionID      uuid.UUID
	LeaseFor       time.Duration
	Limit          int
}

// RecordResult writes a job's outcome in one guarded statement and reports whether this call
// was the one that recorded it. The guard is the fence plus the job not already being
// terminal, so a superseded execution cannot overwrite the current one and a resent result
// cannot be recorded twice.
//
// Callers acknowledge only after this returns successfully. Acknowledging first would let a
// relay stop resending a result that was never durably stored.
func (p *Placements) RecordResult(
	ctx context.Context,
	organization tenancy.Organization,
	fence JobFence,
	outcome JobOutcome,
) (ResultRefusal, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return 0, err
	}

	tag, err := pool.Exec(ctx, `
		UPDATE relay_job
		   SET status           = $4,
		       result           = $5,
		       terminal_at      = now(),
		       lease_expires_at = NULL,
		       lease_session    = NULL
		 WHERE job_id        = $1
		   AND organization  = $2
		   AND status        = 1
		   AND lease_session = $3
		   AND lease_epoch   = $6`,
		fence.JobID, organization.String(), fence.LeaseSession,
		int16(outcome.Status), outcome.Result, fence.LeaseEpoch)
	if err != nil {
		return 0, fmt.Errorf("recording job result: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return 0, nil
	}
	return p.explainRefusedResult(ctx, organization, fence)
}

// JobOutcome is what an execution produced.
type JobOutcome struct {
	Status JobStatus
	Result []byte
}

// explainRefusedResult reads why the guarded update matched nothing. The distinction between
// a resend and a superseded execution matters to the caller here — unlike enrolment, where
// telling reasons apart would help an attacker — because a relay must stop resending in one
// case and an operator must be alerted in the other.
func (p *Placements) explainRefusedResult(
	ctx context.Context,
	organization tenancy.Organization,
	fence JobFence,
) (ResultRefusal, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return 0, err
	}

	var (
		status JobStatus
		epoch  int64
	)
	err = pool.QueryRow(ctx, `
		SELECT status, lease_epoch FROM relay_job WHERE job_id = $1 AND organization = $2`,
		fence.JobID, organization.String()).Scan(&status, &epoch)
	switch {
	case err != nil && errors.Is(err, pgx.ErrNoRows):
		return ResultJobUnknown, ErrResultRefused
	case err != nil:
		return 0, fmt.Errorf("auditing refused result: %w", err)
	case status != JobPending && status != JobLeased:
		return ResultAlreadyRecorded, ErrResultRefused
	default:
		// The job is live but the fence did not match, so this result belongs to an
		// execution that has since lost the lease.
		return ResultFenceSuperseded, ErrResultRefused
	}
}

// SweepExpiredLeases returns work whose lease ran out to pending, so a relay that
// disappeared mid-job does not strand it forever. Terminal jobs are never touched: their
// outcome is already recorded, and re-running them would be the duplicate execution the
// fence exists to prevent.
func (p *Placements) SweepExpiredLeases(
	ctx context.Context, organization tenancy.Organization,
) (int64, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return 0, err
	}
	tag, err := pool.Exec(ctx, `
		UPDATE relay_job
		   SET status           = 0,
		       lease_session    = NULL,
		       lease_expires_at = NULL
		 WHERE organization     = $1
		   AND status           = 1
		   AND lease_expires_at <= now()`,
		organization.String())
	if err != nil {
		return 0, fmt.Errorf("sweeping expired leases: %w", err)
	}
	return tag.RowsAffected(), nil
}
