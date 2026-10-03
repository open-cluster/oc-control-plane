package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

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
	pool, err := p.Pool(organization)
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
