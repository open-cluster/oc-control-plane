package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// Rounds, their leases, and the fence every write inside one passes through.
//
// The lease is the relay job's lease with a different table under it: a server-clock expiry, a
// session, and a generation raised on every claim. That is deliberate rather than convenient —
// both halves of this problem are "one worker at a time, survive a restart, refuse a write from
// the execution that lost", and a second concurrency model for it would be two places for the same
// bug to live.

// roundColumns is the round's shape, written down once.
const roundColumns = `
	round_id, investigation_id, ordinal, brief, controls, plan,
	planner_version, model_version, prompt_version, schema_version, investigator_version,
	outcome, spend_requests, spend_result_bytes, spend_tokens, spend_micro_cents,
	lease_session, lease_epoch, started_at, terminal_at`

// fencedRound guards every write inside a round. It is a SELECT rather than a check performed
// first and acted on second, so a lease that expires between the two cannot let a write through.
//
// The four bound parameters are always $1 round, $2 organization, $3 lease session, $4 generation,
// in every statement that uses it, so a caller cannot get the order wrong in one place only.
const fencedRound = `
	  FROM investigation_round
	 WHERE investigation_round.round_id      = $1
	   AND investigation_round.organization  = $2
	   AND investigation_round.lease_session = $3
	   AND investigation_round.lease_epoch   = $4
	   AND investigation_round.outcome      IS NULL`

// OpenRound adds a bounded execution to a case, pinning everything it will run under.
//
// The ordinal is computed inside the insert. Reading the highest and adding one outside it would
// let two rounds opened at once take the same number, and the ordinal is what an export names when
// it says which rounds it includes.
// A reinvestigation records itself here, in the transaction that opens the round. The first
// round of a case does not: it is part of the case being opened, and OpenInvestigation already
// recorded that. Two events for one operator act would make the trail read as two acts.
func (p *Placements) OpenRound(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	opening investigation.Opening,
) (investigation.Round, error) {
	if !principal.MemberOf(organization) {
		return investigation.Round{}, ErrNotAMember
	}
	pool, err := p.Pool(organization)
	if err != nil {
		return investigation.Round{}, err
	}

	controls, err := json.Marshal(opening.Controls)
	if err != nil {
		return investigation.Round{}, fmt.Errorf("encoding a control snapshot: %w", err)
	}
	plan, err := json.Marshal(opening.Plan)
	if err != nil {
		return investigation.Round{}, fmt.Errorf("encoding a plan snapshot: %w", err)
	}

	transaction, err := pool.Begin(ctx)
	if err != nil {
		return investigation.Round{}, fmt.Errorf("beginning a round: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	row := transaction.QueryRow(ctx, `
		INSERT INTO investigation_round
			(round_id, organization, investigation_id, ordinal, controls, plan,
			 planner_version, model_version, prompt_version, schema_version, investigator_version)
		SELECT $1, $2, investigation.investigation_id,
		       (SELECT coalesce(max(ordinal), 0) + 1
		          FROM investigation_round
		         WHERE investigation_round.investigation_id = investigation.investigation_id),
		       $4, $5, $6, $7, $8, $9, $10
		  FROM investigation
		 WHERE investigation.investigation_id = $3
		   AND investigation.organization     = $2
		   -- A concluded, abstained or failed case may be reinvestigated: reinvestigation adds a
		   -- round to the SAME case rather than creating a second one, which is what keeps the
		   -- identity, the URL and the permalink an engineer shared (ADR-013).
		   --
		   -- A CANCELLED case may not. Cancelling and then finding a new round opened against it
		   -- would make the cancellation advisory, and an operator who stopped a run to stop it
		   -- costing money would be wrong about having stopped it.
		   AND investigation.lifecycle       <> $11
		RETURNING `+roundColumns,
		uuid.New(), organization.String(), opening.InvestigationID, controls, plan,
		opening.Versions.Planner, opening.Versions.Model, opening.Versions.PromptVersion,
		opening.Versions.SchemaVersion, opening.Versions.Investigator,
		int16(investigation.LifecycleCancelled))

	opened, err := scanRound(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return investigation.Round{}, investigation.ErrUnknown
	}
	if err != nil {
		return investigation.Round{}, fmt.Errorf("opening a round: %w", err)
	}

	if _, err = transaction.Exec(ctx, `
		UPDATE investigation
		   SET current_round = $3, lifecycle = $4, terminal_at = NULL
		 WHERE investigation_id = $1 AND organization = $2`,
		opening.InvestigationID, organization.String(), opened.Ordinal,
		int16(investigation.LifecyclePending)); err != nil {
		return investigation.Round{}, fmt.Errorf("opening a round: %w", err)
	}

	if opening.Reinvestigation {
		if err = writeEvent(ctx, transaction, audit.Event{
			Organization:  organization.String(),
			Actor:         principal.Actor(),
			Action:        audit.ActionReinvestigated,
			Target:        audit.Target{Kind: audit.TargetInvestigation, ID: opening.InvestigationID.String()},
			Outcome:       audit.OutcomeAllowed,
			SourceAddress: principal.SourceAddress(),
			RequestID:     principal.RequestID(),
			Detail:        audit.Detail{"round": opened.Ordinal},
		}); err != nil {
			return investigation.Round{}, err
		}
	}

	if err = transaction.Commit(ctx); err != nil {
		return investigation.Round{}, fmt.Errorf("committing a round: %w", err)
	}
	return opened, nil
}

// ClaimRounds leases work for one worker session and returns each round with the case it belongs
// to.
//
// The transition to leased commits before anything runs. A crash between claiming and working
// leaves a leased round whose lease expires and is swept, which is recoverable; running first and
// claiming after would leave work in progress and unrecorded, which is not.
func (p *Placements) ClaimRounds(
	ctx context.Context, organization tenancy.Organization, claim investigation.RoundClaim,
) ([]investigation.Claimed, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return nil, err
	}

	rows, err := pool.Query(ctx, `
		UPDATE investigation_round
		   SET lease_session    = $2,
		       -- Raised on every claim, so a write from the execution that lost this round is
		       -- refused rather than recorded.
		       lease_epoch      = lease_epoch + 1,
		       lease_expires_at = now() + $3::interval,
		       heartbeat_at     = now()
		 WHERE round_id IN (
		       SELECT round_id
		         FROM investigation_round
		        WHERE organization = $1
		          AND outcome     IS NULL
		          AND (lease_session IS NULL OR lease_expires_at <= now())
		        ORDER BY started_at
		        -- Capacity is a ceiling on what this session holds at once rather than a batch
		        -- size, so what it already holds is subtracted. Leases that have run out are not
		        -- counted: they are claimable again, and may well be the rows below.
		        LIMIT GREATEST($4 - (SELECT count(*)
		                               FROM investigation_round held
		                              WHERE held.organization     = $1
		                                AND held.lease_session    = $2
		                                AND held.outcome         IS NULL
		                                AND held.lease_expires_at > now()), 0)
		        FOR UPDATE SKIP LOCKED)
		RETURNING `+roundColumns,
		organization.String(), claim.SessionID, claim.LeaseFor.String(), claim.Capacity)
	if err != nil {
		return nil, fmt.Errorf("claiming investigation rounds: %w", err)
	}

	var claimed []investigation.Round
	for rows.Next() {
		round, scanErr := scanRound(rows)
		if scanErr != nil {
			rows.Close()
			return nil, fmt.Errorf("reading a claimed round: %w", scanErr)
		}
		claimed = append(claimed, round)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("claiming investigation rounds: %w", err)
	}

	// The case is read after the claim rather than joined into it, because a claim is an UPDATE
	// and joining a second table into its RETURNING would make the statement decide two things at
	// once. The worker needs the whole case anyway.
	held := make([]investigation.Claimed, 0, len(claimed))
	for _, round := range claimed {
		found, readErr := p.Investigation(ctx, organization, round.InvestigationID)
		if readErr != nil {
			return nil, fmt.Errorf("reading the case a claimed round belongs to: %w", readErr)
		}
		held = append(held, investigation.Claimed{Investigation: found, Round: round})
	}
	return held, nil
}

// RenewRoundLease extends a lease this session holds.
//
// It moves only the lease columns, which the case-version trigger deliberately ignores. A
// heartbeat is not a change within the case, and a version that advanced on one would tell every
// polling client that something happened every few seconds — which is the same defect as a version
// that never advances, arriving from the other side.
func (p *Placements) RenewRoundLease(
	ctx context.Context, organization tenancy.Organization,
	fence investigation.Fence, extend time.Duration,
) error {
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}
	tag, err := pool.Exec(ctx, `
		UPDATE investigation_round
		   SET lease_expires_at = now() + $5::interval, heartbeat_at = now()
		 WHERE round_id      = $1
		   AND organization  = $2
		   AND lease_session = $3
		   AND lease_epoch   = $4
		   AND outcome      IS NULL`,
		fence.RoundID, organization.String(), fence.LeaseSession, fence.LeaseEpoch,
		extend.String())
	if err != nil {
		return fmt.Errorf("renewing a round lease: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return investigation.ErrLeaseLost
	}
	return nil
}

// RecordBrief pins the deterministic orientation and moves the case on to reasoning.
//
// The brief column is NULL until this runs, which is what makes "the brief exists before any
// hypothesis does" an observable fact rather than an assertion about the order of two function
// calls.
func (p *Placements) RecordBrief(
	ctx context.Context, organization tenancy.Organization,
	fence investigation.Fence, brief investigation.Brief,
) error {
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(brief)
	if err != nil {
		return fmt.Errorf("encoding a brief: %w", err)
	}

	transaction, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning a brief: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	tag, err := transaction.Exec(ctx, `
		UPDATE investigation_round
		   SET brief = $5
		 WHERE round_id      = $1
		   AND organization  = $2
		   AND lease_session = $3
		   AND lease_epoch   = $4
		   AND outcome      IS NULL`,
		fence.RoundID, organization.String(), fence.LeaseSession, fence.LeaseEpoch, encoded)
	if err != nil {
		return fmt.Errorf("recording a brief: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return investigation.ErrLeaseLost
	}

	if err = advanceLifecycle(ctx, transaction, organization, fence,
		investigation.LifecycleReasoning); err != nil {
		return err
	}
	if err = transaction.Commit(ctx); err != nil {
		return fmt.Errorf("committing a brief: %w", err)
	}
	return nil
}

// AdvanceLifecycle moves the case between the states a running round passes through.
func (p *Placements) AdvanceLifecycle(
	ctx context.Context, organization tenancy.Organization,
	fence investigation.Fence, to investigation.Lifecycle,
) error {
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}
	return advanceLifecycle(ctx, pool, organization, fence, to)
}

// executor is what both a pool and a transaction offer, so the guarded statements below can run
// either alone or as part of a larger write without a second copy of the SQL.
type executor interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// advanceLifecycle moves the case a fenced round belongs to. It goes through the round rather than
// naming the case, so an execution that lost its lease cannot move a case it no longer owns.
func advanceLifecycle(
	ctx context.Context, on executor, organization tenancy.Organization,
	fence investigation.Fence, to investigation.Lifecycle,
) error {
	tag, err := on.Exec(ctx, `
		UPDATE investigation
		   SET lifecycle = $5
		  FROM investigation_round
		 WHERE investigation.investigation_id     = investigation_round.investigation_id
		   AND investigation.organization         = $2
		   AND investigation_round.round_id       = $1
		   AND investigation_round.organization   = $2
		   AND investigation_round.lease_session  = $3
		   AND investigation_round.lease_epoch    = $4
		   AND investigation_round.outcome       IS NULL`,
		fence.RoundID, organization.String(), fence.LeaseSession, fence.LeaseEpoch, int16(to))
	if err != nil {
		return fmt.Errorf("advancing an investigation lifecycle: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return investigation.ErrLeaseLost
	}
	return nil
}

// RecordSpend adds what a pass consumed to the round and to the case. Both are kept: the round's
// figure is what a reviewer scores, and the case's is what an operator prices.
func (p *Placements) RecordSpend(
	ctx context.Context, organization tenancy.Organization,
	fence investigation.Fence, spend investigation.Spend,
) error {
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}

	transaction, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning a spend: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	tag, err := transaction.Exec(ctx, `
		UPDATE investigation_round
		   SET spend_requests     = spend_requests + $5,
		       spend_result_bytes = spend_result_bytes + $6,
		       spend_tokens       = spend_tokens + $7,
		       spend_micro_cents  = spend_micro_cents + $8
		 WHERE round_id      = $1
		   AND organization  = $2
		   AND lease_session = $3
		   AND lease_epoch   = $4
		   AND outcome      IS NULL`,
		fence.RoundID, organization.String(), fence.LeaseSession, fence.LeaseEpoch,
		spend.Requests, spend.ResultBytes, spend.Tokens, spend.MicroCents)
	if err != nil {
		return fmt.Errorf("recording spend: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return investigation.ErrLeaseLost
	}

	// Qualified on both sides: the round carries columns of the same names, and an unqualified
	// reference here reads as ambiguous rather than as the case's.
	if _, err = transaction.Exec(ctx, `
		UPDATE investigation
		   SET spend_tokens      = investigation.spend_tokens + $5,
		       spend_micro_cents = investigation.spend_micro_cents + $6,
		       spend_millis      = investigation.spend_millis + $7
		  FROM investigation_round
		 WHERE investigation.investigation_id    = investigation_round.investigation_id
		   AND investigation.organization        = $2
		   AND investigation_round.round_id      = $1
		   AND investigation_round.organization  = $2
		   AND investigation_round.lease_session = $3
		   AND investigation_round.lease_epoch   = $4`,
		fence.RoundID, organization.String(), fence.LeaseSession, fence.LeaseEpoch,
		spend.Tokens, spend.MicroCents, spend.Duration.Milliseconds()); err != nil {
		return fmt.Errorf("recording case spend: %w", err)
	}

	if err = transaction.Commit(ctx); err != nil {
		return fmt.Errorf("committing spend: %w", err)
	}
	return nil
}

// FinishRound stamps a round terminal and projects its outcome onto the case.
//
// Both happen in one transaction. A round marked finished whose case still reads as running would
// tell a client to keep polling something nothing is working on, and a case marked terminal over a
// round still holding a lease would let that execution keep writing.
func (p *Placements) FinishRound(
	ctx context.Context, organization tenancy.Organization,
	fence investigation.Fence, finish investigation.Finish,
) error {
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}

	transaction, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning a round finish: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	var exhausted *int16
	if finish.Exhausted != 0 {
		value := int16(finish.Exhausted)
		exhausted = &value
	}

	tag, err := transaction.Exec(ctx, `
		UPDATE investigation_round
		   SET outcome          = $5,
		       reached_limit = $6,
		       terminal_at      = now(),
		       lease_session    = NULL,
		       lease_expires_at = NULL
		 WHERE round_id      = $1
		   AND organization  = $2
		   AND lease_session = $3
		   AND lease_epoch   = $4
		   AND outcome      IS NULL`,
		fence.RoundID, organization.String(), fence.LeaseSession, fence.LeaseEpoch,
		int16(finish.Outcome), exhausted)
	if err != nil {
		return fmt.Errorf("finishing a round: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return investigation.ErrLeaseLost
	}

	// The lease is now released, so the case update is guarded on the round's identity and outcome
	// rather than on a lease that no longer exists.
	if _, err = transaction.Exec(ctx, `
		UPDATE investigation
		   SET lifecycle = $3, terminal_at = now()
		  FROM investigation_round
		 WHERE investigation.investigation_id   = investigation_round.investigation_id
		   AND investigation.organization       = $2
		   AND investigation_round.round_id     = $1
		   AND investigation_round.organization = $2`,
		fence.RoundID, organization.String(),
		int16(investigation.LifecycleFor(finish.Outcome))); err != nil {
		return fmt.Errorf("projecting a round outcome onto its case: %w", err)
	}

	if err = transaction.Commit(ctx); err != nil {
		return fmt.Errorf("committing a round finish: %w", err)
	}
	return nil
}

// Round reads one round, scoped to the tenant.
func (p *Placements) Round(
	ctx context.Context, organization tenancy.Organization, roundID uuid.UUID,
) (investigation.Round, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return investigation.Round{}, err
	}
	row := pool.QueryRow(ctx, `
		SELECT `+roundColumns+`
		  FROM investigation_round
		 WHERE round_id = $1 AND organization = $2`,
		roundID, organization.String())
	found, err := scanRound(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return investigation.Round{}, investigation.ErrRoundUnknown
	}
	if err != nil {
		return investigation.Round{}, fmt.Errorf("reading a round: %w", err)
	}
	return found, nil
}

// Rounds reads a case's rounds oldest first, which is the order an export names them in.
func (p *Placements) Rounds(
	ctx context.Context, organization tenancy.Organization, investigationID uuid.UUID,
) ([]investigation.Round, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, `
		SELECT `+roundColumns+`
		  FROM investigation_round
		 WHERE investigation_id = $1 AND organization = $2
		 ORDER BY ordinal`,
		investigationID, organization.String())
	if err != nil {
		return nil, fmt.Errorf("reading rounds: %w", err)
	}
	defer rows.Close()

	var found []investigation.Round
	for rows.Next() {
		round, scanErr := scanRound(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("reading a round: %w", scanErr)
		}
		found = append(found, round)
	}
	return found, rows.Err()
}

func scanRound(row scanned) (investigation.Round, error) {
	var (
		round      investigation.Round
		brief      []byte
		controls   []byte
		plan       []byte
		outcome    *int16
		session    *uuid.UUID
		terminalAt *time.Time
	)
	if err := row.Scan(&round.ID, &round.InvestigationID, &round.Ordinal, &brief, &controls, &plan,
		&round.Versions.Planner, &round.Versions.Model, &round.Versions.PromptVersion,
		&round.Versions.SchemaVersion, &round.Versions.Investigator,
		&outcome, &round.Spend.Requests, &round.Spend.ResultBytes, &round.Spend.Tokens,
		&round.Spend.MicroCents,
		&session, &round.LeaseEpoch, &round.StartedAt, &terminalAt); err != nil {
		return investigation.Round{}, err
	}
	if len(brief) > 0 {
		if err := json.Unmarshal(brief, &round.Brief); err != nil {
			return investigation.Round{}, fmt.Errorf("decoding a brief: %w", err)
		}
	}
	if err := json.Unmarshal(controls, &round.Controls); err != nil {
		return investigation.Round{}, fmt.Errorf("decoding a control snapshot: %w", err)
	}
	if err := json.Unmarshal(plan, &round.Plan); err != nil {
		return investigation.Round{}, fmt.Errorf("decoding a plan snapshot: %w", err)
	}
	if outcome != nil {
		round.Outcome = investigation.RoundOutcome(*outcome)
	}
	if session != nil {
		round.LeaseSession = *session
	}
	if terminalAt != nil {
		round.TerminalAt = *terminalAt
		round.Spend.Duration = terminalAt.Sub(round.StartedAt)
	}
	return round, nil
}
