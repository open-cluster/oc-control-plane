package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

type JobClaim struct {
	RegistrationID uuid.UUID
	SessionID      uuid.UUID
	LeaseFor       time.Duration
	Capacity       int
}

func (p *Database) ClaimJobs(
	ctx context.Context,
	organization uuid.UUID,
	claim JobClaim,
) ([]RelayJob, error) {
	// Claiming commits before delivery so a crash leaves recoverable leased work. Each claim
	// raises the epoch, fencing results from executions whose lease expired.
	pool, err := p.poolForOrganization(organization)
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
		        WHERE org_id    = $1
		          AND registration_id = $2
		          AND (status = 0 OR (status = 1 AND lease_expires_at <= now()))
		        ORDER BY created_at
		        -- Capacity is a ceiling on what this session holds at once, not a batch size,
		        -- so what it already holds is subtracted. Claiming a full batch every round
		        -- would lease a whole backlog to one relay within minutes and strand it there.
		        -- Leases that have run out are not counted: they are claimable again, and the
		        -- rows below may well be those very jobs.
		        LIMIT GREATEST($5 - (SELECT count(*)
		                               FROM relay_job held
		                              WHERE held.org_id           = $1
		                                AND held.lease_session    = $3
		                                AND held.status           = 1
		                                AND held.lease_expires_at > now()), 0)
		        -- Concurrent claimers take disjoint work instead of blocking on each other.
		        FOR UPDATE SKIP LOCKED)
		RETURNING job_id, integration_id, registration_id, capability_id, capability_version,
		          arguments, lease_session, lease_epoch`,
		organization, claim.RegistrationID, claim.SessionID,
		claim.LeaseFor.String(), claim.Capacity)
	if err != nil {
		return nil, fmt.Errorf("claiming jobs: %w", err)
	}
	defer rows.Close()

	var claimed []RelayJob
	for rows.Next() {
		var job RelayJob
		if err = rows.Scan(&job.ID, &job.IntegrationID, &job.RegistrationID, &job.CapabilityID,
			&job.CapabilityVersion, &job.Arguments, &job.LeaseSession, &job.LeaseEpoch); err != nil {
			return nil, fmt.Errorf("reading claimed job: %w", err)
		}
		claimed = append(claimed, job)
	}
	return claimed, rows.Err()
}

type InFlightJob struct {
	JobID      uuid.UUID
	LeaseEpoch int64
}

type LeaseAdoption struct {
	RegistrationID uuid.UUID
	SessionID      uuid.UUID
	LeaseFor       time.Duration
	InFlight       []InFlightJob
}

func (p *Database) AdoptInFlightLeases(
	ctx context.Context, organization uuid.UUID, adoption LeaseAdoption,
) ([]uuid.UUID, error) {
	// Adoption may only move an existing lease for this registration at the declared epoch;
	// the epoch stays unchanged so the result already in flight remains valid.
	if len(adoption.InFlight) == 0 {
		return nil, nil
	}
	pool, err := p.poolForOrganization(organization)
	if err != nil {
		return nil, err
	}

	jobs := make([]uuid.UUID, 0, len(adoption.InFlight))
	epochs := make([]int64, 0, len(adoption.InFlight))
	for _, declared := range adoption.InFlight {
		jobs = append(jobs, declared.JobID)
		epochs = append(epochs, declared.LeaseEpoch)
	}

	rows, err := pool.Query(ctx, `
		UPDATE relay_job
		   SET lease_session    = $3,
		       lease_expires_at = now() + $4::interval
		  FROM unnest($5::uuid[], $6::bigint[]) AS declared(job_id, lease_epoch)
		 WHERE relay_job.job_id          = declared.job_id
		   AND relay_job.lease_epoch     = declared.lease_epoch
		   AND relay_job.org_id          = $1
		   AND relay_job.registration_id = $2
		   AND relay_job.status          = 1
		RETURNING relay_job.job_id`,
		organization, adoption.RegistrationID, adoption.SessionID,
		adoption.LeaseFor.String(), jobs, epochs)
	if err != nil {
		return nil, fmt.Errorf("adopting in-flight leases: %w", err)
	}
	defer rows.Close()

	var adopted []uuid.UUID
	for rows.Next() {
		var job uuid.UUID
		if err = rows.Scan(&job); err != nil {
			return nil, fmt.Errorf("reading an adopted lease: %w", err)
		}
		adopted = append(adopted, job)
	}
	return adopted, rows.Err()
}

func (p *Database) ReleaseStrandedLeases(
	ctx context.Context,
	organization uuid.UUID,
	registrationID, holder uuid.UUID,
) (int64, error) {
	// Call only after receiving a complete roster. The fence prevents corruption if the roster
	// is wrong, but an omitted execution would be repeated.
	pool, err := p.poolForOrganization(organization)
	if err != nil {
		return 0, err
	}
	tag, err := pool.Exec(ctx, `
		UPDATE relay_job
		   SET status           = 0,
		       lease_session    = NULL,
		       lease_expires_at = NULL,
		       -- Raised here rather than left for the next claim. Releasing a lease ends that
		       -- execution's claim on the job, and until the generation moves, a late result
		       -- from it reads as an execution nothing has superseded — so the relay would be
		       -- told nothing, keep the result, and resend it forever against a job that is
		       -- already on its way to being run again.
		       lease_epoch      = lease_epoch + 1
		 WHERE org_id     = $1
		   AND registration_id  = $2
		   AND status           = 1
		   AND lease_session IS DISTINCT FROM $3`,
		organization, registrationID, holder)
	if err != nil {
		return 0, fmt.Errorf("releasing stranded leases: %w", err)
	}
	return tag.RowsAffected(), nil
}
