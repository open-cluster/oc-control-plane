package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

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
func (p *Database) RequestJobCancellation(
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
		   AND org_id = $2
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
func (p *Database) PendingCancellations(
	ctx context.Context, organization tenancy.Organization, sessionID uuid.UUID,
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
