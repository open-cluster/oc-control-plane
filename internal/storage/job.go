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
	// ResultFenceSuperseded means a later generation of the lease exists, so another execution
	// has taken over this job. Recording this result would overwrite the outcome of the
	// execution that owns the work now.
	ResultFenceSuperseded
	// ResultJobUnknown means no such job exists for this organization.
	ResultJobUnknown
	// ResultLeaseNotHeld means the generation still matches — nothing has superseded this
	// execution — but the lease is held by a different session. It is kept apart from a
	// supersession because the two call for opposite answers: a superseded relay must stop
	// resending, while this one must not, since no other execution is going to produce this
	// result and its lease can still be adopted.
	ResultLeaseNotHeld
)

func (r ResultRefusal) String() string {
	switch r {
	case ResultAlreadyRecorded:
		return "already recorded"
	case ResultFenceSuperseded:
		return "lease superseded"
	case ResultJobUnknown:
		return "job unknown"
	case ResultLeaseNotHeld:
		return "lease held by another session"
	default:
		return "unrecognised"
	}
}

// Job is a unit of work as the control plane holds it.
type Job struct {
	ID             uuid.UUID
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
		RETURNING job_id, registration_id, capability_id, capability_version,
		          arguments, lease_session, lease_epoch`,
		organization.String(), claim.RegistrationID, claim.SessionID,
		claim.LeaseFor.String(), claim.Capacity)
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
	// Capacity is the most work this session may hold at once, not the most it may take in
	// one call. What it already holds is subtracted before anything more is leased.
	Capacity int
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

// explainRefusedResult reads why the guarded update matched nothing. The distinctions matter
// to the caller here — unlike enrolment, where telling reasons apart would help an attacker —
// because each one calls for a different answer to the relay, and getting that answer wrong
// either discards a result or repeats one.
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
	case epoch > fence.LeaseEpoch:
		// A later generation exists, so the job has been claimed again and this result belongs
		// to an execution that lost it.
		return ResultFenceSuperseded, ErrResultRefused
	default:
		// The generation still stands. Nothing superseded this execution; the lease is simply
		// held by another session, or by none. Claiming supersession here would tell a relay
		// to discard a result that no other execution is going to produce.
		return ResultLeaseNotHeld, ErrResultRefused
	}
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

// JobCancellation is what asking a job to stop actually did. The three cases are genuinely
// different events, and a caller that cannot tell them apart either reports a stop that never
// happened or hides an outcome that already did.
type JobCancellation int

const (
	// CancellationRefused means the job had already reached an outcome. Nothing changed: a
	// result that already exists is not undone by asking for a stop.
	CancellationRefused JobCancellation = iota + 1
	// CancellationRecorded means the job had not started, so it was cancelled outright. No
	// relay is involved, because nothing is executing it and nothing else could ever finish it.
	CancellationRecorded
	// CancellationRequested means the job is executing. The request is advisory: the job stays
	// leased and its terminal outcome still arrives from the relay, because there is exactly
	// one write path into job truth and this is not it.
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

// RequestJobCancellation asks for a job to stop and reports what that meant for this job.
//
// Both cases are decided in one statement, so a job cannot start executing between reading
// its state and acting on it — which would otherwise cancel a job outright while a relay was
// already running it, and leave that execution's result with nowhere to go.
func (p *Placements) RequestJobCancellation(
	ctx context.Context, organization tenancy.Organization, jobID uuid.UUID,
) (JobCancellation, error) {
	pool, err := p.Pool(organization)
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
		   AND organization = $2
		   AND status IN (0, 1)
		RETURNING status`,
		jobID, organization.String()).Scan(&resulting)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Either terminal already or no such job. Both mean there is nothing here to stop.
		return CancellationRefused, nil
	case err != nil:
		return 0, fmt.Errorf("requesting job cancellation: %w", err)
	case resulting == JobCancelled:
		return CancellationRecorded, nil
	default:
		return CancellationRequested, nil
	}
}

// PendingCancellations lists the executing jobs a session has been asked to stop. It is scoped
// to the session holding the lease, so no relay is ever told to stop work it is not executing.
func (p *Placements) PendingCancellations(
	ctx context.Context, organization tenancy.Organization, sessionID uuid.UUID,
) ([]JobFence, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return nil, err
	}

	rows, err := pool.Query(ctx, `
		SELECT job_id, lease_session, lease_epoch
		  FROM relay_job
		 WHERE organization        = $1
		   AND lease_session       = $2
		   AND status              = 1
		   AND cancel_requested_at IS NOT NULL`,
		organization.String(), sessionID)
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
