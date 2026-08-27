package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

// The investigation lease. Every timestamp here is the SERVER's clock — now() in the
// statement, never a value a worker computed — so a worker with a skewed clock cannot hold
// a lease longer than it was given or lose one it still has.
var _ investigation.Leases = (*Database)(nil)

// ClaimInvestigation leases the oldest waiting investigation this worker may take.
//
// The per-organization ceiling is a subquery over the candidate's own tenant rather than a
// number the caller tracks, because the number has to be true across every replica and no
// process can see the others. Two claimers racing can still overshoot it by one — each
// counts before the other commits — which is the same bounded race the process-wide
// ceiling has always had, and is bounded again by every investigation's own budgets.
func (p *Database) ClaimInvestigation(
	ctx context.Context, claim investigation.Claim,
) (tenancy.Organization, investigation.Investigation, bool, error) {
	row := p.pool.QueryRow(ctx, `
			UPDATE investigation
			   SET lease_worker       = $1,
			       lease_expires_at   = now() + $2::interval,
			       lease_heartbeat_at = now()
			 WHERE investigation_id = (
			       SELECT waiting.investigation_id
			         FROM investigation waiting
			        WHERE waiting.status       = 1
			          AND waiting.lease_worker = ''
			          AND (SELECT count(*)
			                 FROM investigation executing
			                WHERE executing.org_id           = waiting.org_id
			                  AND executing.status           = 1
			                  AND executing.lease_worker    <> ''
			                  AND executing.lease_expires_at > now()) < $3
			        -- Oldest first, so work is taken up in the order it was asked for.
			        ORDER BY waiting.created_at, waiting.investigation_id
			        LIMIT 1
			        -- Concurrent claimers take different work instead of blocking on each
			        -- other, which is what lets a deployment add a replica and go faster.
			        FOR UPDATE SKIP LOCKED)
			RETURNING org_id, `+investigationColumns,
		claim.Worker, claim.LeaseFor.String(), claim.OrgConcurrent)

	var organization string
	claimed, err := scanClaimedInvestigation(row, &organization)
	if errors.Is(err, pgx.ErrNoRows) {
		return tenancy.Organization{}, investigation.Investigation{}, false, nil
	}
	if err != nil {
		return tenancy.Organization{}, investigation.Investigation{}, false,
			fmt.Errorf("claiming an investigation: %w", err)
	}
	named, err := tenancy.NewOrganization(organization)
	if err != nil {
		return tenancy.Organization{}, investigation.Investigation{}, false,
			fmt.Errorf("a claimed investigation names an unusable organization: %w", err)
	}
	claimed.OrgID = organization
	return named, claimed, true, nil
}

// TakeLease claims one named investigation for the path that already holds the record it
// just created. It refuses one somebody else holds, and one that has already ended.
func (p *Database) TakeLease(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID,
	claim investigation.Claim,
) (bool, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return false, err
	}
	tag, err := pool.Exec(ctx, `
		UPDATE investigation
		   SET lease_worker       = $3,
		       lease_expires_at   = now() + $4::interval,
		       lease_heartbeat_at = now()
		 WHERE investigation_id = $1
		   AND org_id           = $2
		   AND status           = 1
		   -- An unheld lease, or one that has already lapsed. A live lease belongs to
		   -- somebody, and taking it would be the double execution the fence exists to
		   -- prevent.
		   AND (lease_worker = '' OR lease_expires_at <= now())`,
		id, organization.String(), claim.Worker, claim.LeaseFor.String())
	if err != nil {
		return false, fmt.Errorf("taking an investigation lease: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// Heartbeat renews a lease this worker holds.
//
// The worker is part of the WHERE clause, so a lease that was swept and re-claimed by
// somebody else cannot be renewed by the process that lost it. That is the fence: the
// holder learns it is no longer the holder, from the database, rather than continuing to
// write for an investigation another worker is now running.
func (p *Database) Heartbeat(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID,
	claim investigation.Claim,
) (bool, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return false, err
	}
	tag, err := pool.Exec(ctx, `
		UPDATE investigation
		   SET lease_expires_at   = now() + $4::interval,
		       lease_heartbeat_at = now()
		 WHERE investigation_id = $1
		   AND org_id           = $2
		   AND status           = 1
		   AND lease_worker     = $3`,
		id, organization.String(), claim.Worker, claim.LeaseFor.String())
	if err != nil {
		return false, fmt.Errorf("renewing an investigation lease: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// RecoverStale fails every investigation whose lease lapsed and writes each a terminal
// event, so a reader watching one is told it stopped instead of watching forever.
//
// The record and the event are written in ONE transaction. Failing the investigation
// without ending its stream would leave exactly the symptom this exists to cure, one level
// down: a record that says failed and a reader still waiting.
//
// The event's sequence is read from the table rather than held in memory, because the
// process that held the in-memory counter is the process that died.
func (p *Database) RecoverStale(
	ctx context.Context, reason string, limit int,
) (int, error) {
	if limit <= 0 {
		limit = 1
	}
	recovered, err := recoverStaleIn(ctx, p.pool, reason, limit)
	if err != nil {
		return 0, fmt.Errorf("recovering stale investigations: %w", err)
	}
	return recovered, nil
}

// recoverStaleIn recovers up to limit stale investigations.
func recoverStaleIn(
	ctx context.Context, pool *pgxpool.Pool, reason string, limit int,
) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	transaction, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = transaction.Rollback(ctx)
		}
	}()

	// The failing update and the identifiers it touched, in one statement: anything read
	// first and updated after would be a window in which the lease could be renewed.
	rows, err := transaction.Query(ctx, `
		UPDATE investigation
		   SET status           = 3,
		       error            = $1,
		       concluded_at     = now(),
		       lease_worker     = '',
		       lease_expires_at = NULL
		 WHERE investigation_id IN (
		       SELECT lapsed.investigation_id
		         FROM investigation lapsed
		        WHERE lapsed.status           = 1
		          AND lapsed.lease_worker    <> ''
		          AND lapsed.lease_expires_at <= now()
		        ORDER BY lapsed.lease_expires_at
		        LIMIT $2
		        FOR UPDATE SKIP LOCKED)
		RETURNING investigation_id, org_id`, reason, limit)
	if err != nil {
		return 0, fmt.Errorf("failing lapsed investigations: %w", err)
	}

	type stranded struct {
		id           uuid.UUID
		organization string
	}
	var swept []stranded
	for rows.Next() {
		var one stranded
		if err = rows.Scan(&one.id, &one.organization); err != nil {
			rows.Close()
			return 0, fmt.Errorf("reading a lapsed investigation: %w", err)
		}
		swept = append(swept, one)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return 0, fmt.Errorf("failing lapsed investigations: %w", err)
	}

	payload, err := json.Marshal(map[string]any{"reason": reason})
	if err != nil {
		return 0, fmt.Errorf("encoding a recovery reason: %w", err)
	}
	for _, one := range swept {
		// The sequence comes from the TABLE, not from memory: the process that held the
		// in-memory counter for this investigation is the process that died.
		if _, err = transaction.Exec(ctx, `
			INSERT INTO investigation_event (investigation_id, org_id, sequence, type,
			                                 payload)
			SELECT $1, $2,
			       coalesce((SELECT max(sequence)
			                   FROM investigation_event written
			                  WHERE written.investigation_id = $1), 0) + 1,
			       $3, $4`,
			one.id, one.organization, int16(investigation.EventFailed),
			payload); err != nil {
			return 0, fmt.Errorf("ending a lapsed investigation's stream: %w", err)
		}
	}

	if err = transaction.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	committed = true
	return len(swept), nil
}

// scanClaimedInvestigation reads a claimed row, whose first column is the organization it
// belongs to — the claimer asked no tenant in particular and has to be told.
func scanClaimedInvestigation(
	row scanned, organization *string,
) (investigation.Investigation, error) {
	return scanInvestigation(prefixedRow{row: row, first: organization}, "")
}

// prefixedRow lets the shared investigation mapping serve a query that returns one extra
// leading column, without a second copy of an eighteen-column scan.
type prefixedRow struct {
	row   scanned
	first *string
}

func (p prefixedRow) Scan(destination ...any) error {
	return p.row.Scan(append([]any{p.first}, destination...)...)
}
