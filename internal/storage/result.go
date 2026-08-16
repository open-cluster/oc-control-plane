package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/tenancy"
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

// JobOutcome is what an execution produced.
type JobOutcome struct {
	Status JobStatus
	Result []byte
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
		   AND org_id  = $2
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
		SELECT status, lease_epoch FROM relay_job WHERE job_id = $1 AND org_id = $2`,
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
