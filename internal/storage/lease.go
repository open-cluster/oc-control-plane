package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// JobClaim is what a session asks for when it takes work.
type JobClaim struct {
	RegistrationID uuid.UUID
	SessionID      uuid.UUID
	LeaseFor       time.Duration
	// Capacity is the most work this session may hold at once, not the most it may take in
	// one call. What it already holds is subtracted before anything more is leased.
	Capacity int
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
		        -- Capacity is a ceiling on what this session holds at once, not a batch size,
		        -- so what it already holds is subtracted. Claiming a full batch every round
		        -- would lease a whole backlog to one relay within minutes and strand it there.
		        -- Leases that have run out are not counted: they are claimable again, and the
		        -- rows below may well be those very jobs.
		        LIMIT GREATEST($5 - (SELECT count(*)
		                               FROM relay_job held
		                              WHERE held.organization     = $1
		                                AND held.lease_session    = $3
		                                AND held.status           = 1
		                                AND held.lease_expires_at > now()), 0)
		        -- Concurrent claimers take disjoint work instead of blocking on each other.
		        FOR UPDATE SKIP LOCKED)
		RETURNING job_id, connection_id, registration_id, capability_id, capability_version,
		          arguments, lease_session, lease_epoch`,
		organization.String(), claim.RegistrationID, claim.SessionID,
		claim.LeaseFor.String(), claim.Capacity)
	if err != nil {
		return nil, fmt.Errorf("claiming jobs: %w", err)
	}
	defer rows.Close()

	var claimed []Job
	for rows.Next() { // mapping to Job
		var job Job
		if err = rows.Scan(&job.ID, &job.ConnectionID, &job.RegistrationID, &job.CapabilityID,
			&job.CapabilityVersion, &job.Arguments, &job.LeaseSession, &job.LeaseEpoch); err != nil {
			return nil, fmt.Errorf("reading claimed job: %w", err)
		}
		claimed = append(claimed, job)
	}
	return claimed, rows.Err()
}

// InFlightJob is a job a relay says it is still executing, as the relay names it: the job, and
// the generation of the lease it was assigned under. The session is not part of it, because a
// relay never learns which session held its lease.
type InFlightJob struct {
	JobID      uuid.UUID
	LeaseEpoch int64
}

// LeaseAdoption is a reconnected relay's account of what it never stopped doing.
type LeaseAdoption struct {
	RegistrationID uuid.UUID
	SessionID      uuid.UUID
	LeaseFor       time.Duration
	InFlight       []InFlightJob
}

// AdoptInFlightLeases moves the leases for work a relay is still executing onto its new
// session, and reports which jobs were actually adopted.
//
// This is the one place a relay's own account of the world changes durable state, so what it
// cannot do is the point. The declaration has no authority: it can only renew a lease on a job
// already leased to this registration at the generation it names. It cannot create work,
// complete work, take work from another registration, revive work that has finished, or claim
// an execution that superseded its own — in every one of those cases no row matches and
// nothing happens, which is the fence deciding rather than the relay.
//
// The generation is deliberately not raised. Raising it would invalidate the very result the
// relay is holding, which is the thing this exists to preserve.
func (p *Placements) AdoptInFlightLeases(
	ctx context.Context, organization tenancy.Organization, adoption LeaseAdoption,
) ([]uuid.UUID, error) {
	if len(adoption.InFlight) == 0 {
		return nil, nil
	}
	pool, err := p.Pool(organization)
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
		   AND relay_job.organization    = $1
		   AND relay_job.registration_id = $2
		   AND relay_job.status          = 1
		RETURNING relay_job.job_id`,
		organization.String(), adoption.RegistrationID, adoption.SessionID,
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

// ReleaseStrandedLeases returns to pending every job this registration has leased to anything
// other than the given holder, and reports how many.
//
// It is the other half of adoption. A relay has one session, so once its current session has
// adopted everything the relay says it is still executing, whatever is left leased belongs to a
// session that is gone and to an execution the relay is not running. Waiting out a full lease
// on that work would add ten minutes of nothing to every network blip.
//
// The caller must only use this when it knows the relay's account was complete. Releasing work
// a relay is in fact still executing does not corrupt anything — the fence still decides who
// may record — but it does mean that execution's result is refused and the work is done twice.
func (p *Placements) ReleaseStrandedLeases(
	ctx context.Context,
	organization tenancy.Organization,
	registrationID, holder uuid.UUID,
) (int64, error) {
	pool, err := p.Pool(organization)
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
		 WHERE organization     = $1
		   AND registration_id  = $2
		   AND status           = 1
		   AND lease_session IS DISTINCT FROM $3`,
		organization.String(), registrationID, holder)
	if err != nil {
		return 0, fmt.Errorf("releasing stranded leases: %w", err)
	}
	return tag.RowsAffected(), nil
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
