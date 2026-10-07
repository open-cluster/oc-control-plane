package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/jackc/pgx/v5"
)

type StoredJobOutcome struct {
	Status     JobStatus
	Result     []byte
	TerminalAt time.Time
}

func (p *Database) JobOutcome(
	ctx context.Context, organization uuid.UUID, jobID uuid.UUID,
) (StoredJobOutcome, bool, error) {
	pool, err := p.poolForOrganization(organization)
	if err != nil {
		return StoredJobOutcome{}, false, err
	}
	var outcome StoredJobOutcome
	err = pool.QueryRow(ctx, `
		SELECT status, result, coalesce(terminal_at, 'epoch'::timestamptz)
		  FROM relay_job WHERE org_id = $1 AND job_id = $2`,
		organization, jobID).Scan(&outcome.Status, &outcome.Result, &outcome.TerminalAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return StoredJobOutcome{}, false, nil
	}
	if err != nil {
		return StoredJobOutcome{}, false, fmt.Errorf("reading job outcome: %w", err)
	}
	terminal := outcome.Status == JobSucceeded || outcome.Status == JobFailed || outcome.Status == JobCancelled
	return outcome, terminal, nil
}

var ErrResultRefused = errors.New("job result refused")

type ResultRefusal int

const (
	ResultAlreadyRecorded ResultRefusal = iota + 1
	ResultFenceSuperseded
	ResultJobUnknown
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

type JobOutcome struct {
	Status JobStatus
	Result []byte
}

func (p *Database) RecordResult(
	ctx context.Context,
	organization uuid.UUID,
	fence JobFence,
	outcome JobOutcome,
) (ResultRefusal, error) {
	pool, err := p.poolForOrganization(organization)
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
		fence.JobID, organization, fence.LeaseSession,
		int16(outcome.Status), outcome.Result, fence.LeaseEpoch)
	if err != nil {
		return 0, fmt.Errorf("recording job result: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return 0, nil
	}
	return p.explainRefusedResult(ctx, organization, fence)
}

func (p *Database) explainRefusedResult(
	ctx context.Context,
	organization uuid.UUID,
	fence JobFence,
) (ResultRefusal, error) {
	pool, err := p.poolForOrganization(organization)
	if err != nil {
		return 0, err
	}

	var (
		status JobStatus
		epoch  int64
	)
	err = pool.QueryRow(ctx, `
		SELECT status, lease_epoch FROM relay_job WHERE job_id = $1 AND org_id = $2`,
		fence.JobID, organization).Scan(&status, &epoch)
	switch {
	case err != nil && errors.Is(err, pgx.ErrNoRows):
		return ResultJobUnknown, ErrResultRefused
	case err != nil:
		return 0, fmt.Errorf("auditing refused result: %w", err)
	case status != JobPending && status != JobLeased:
		return ResultAlreadyRecorded, ErrResultRefused
	case epoch > fence.LeaseEpoch:
		return ResultFenceSuperseded, ErrResultRefused
	default:
		return ResultLeaseNotHeld, ErrResultRefused
	}
}
