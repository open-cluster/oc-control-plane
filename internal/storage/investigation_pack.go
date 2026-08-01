package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// Writing the case pack: the hypotheses a round held, the reads it asked for and why, the evidence
// and gaps those produced, and the outcome it reached. Every write here passes through the round's
// fence, so an execution that lost its lease writes nothing.

// RecordHypotheses appends what the planner proposed and returns them with their identities and
// ordinals.
//
// Ordinals are assigned by the database rather than by the caller. The planner's ranking is an
// ordinal and never a score, and letting it supply the number would make it possible to publish
// one that means something else.
func (p *Placements) RecordHypotheses(
	ctx context.Context, organization tenancy.Organization,
	fence investigation.Fence, proposed []investigation.Hypothesis,
) ([]investigation.Hypothesis, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return nil, err
	}

	recorded := make([]investigation.Hypothesis, 0, len(proposed))
	for _, hypothesis := range proposed {
		row := pool.QueryRow(ctx, `
			INSERT INTO investigation_hypothesis
				(hypothesis_id, organization, investigation_id, round_id, ordinal,
				 statement, falsifies, state, set_aside_reason)
			SELECT $5, $2, investigation_round.investigation_id, $1,
			       (SELECT coalesce(max(ordinal), 0) + 1
			          FROM investigation_hypothesis existing
			         WHERE existing.round_id = $1),
			       $6, $7, $8, $9`+fencedRound+`
			RETURNING hypothesis_id, investigation_id, round_id, ordinal, statement, falsifies,
			          state, set_aside_reason, proposed_at, updated_at`,
			fence.RoundID, organization.String(), fence.LeaseSession, fence.LeaseEpoch,
			uuid.New(), hypothesis.Statement, hypothesis.Falsifies,
			int16(hypothesis.State), hypothesis.SetAsideReason)

		written, scanErr := scanHypothesis(row)
		if errors.Is(scanErr, pgx.ErrNoRows) {
			return nil, investigation.ErrLeaseLost
		}
		if scanErr != nil {
			return nil, fmt.Errorf("recording a hypothesis: %w", scanErr)
		}
		recorded = append(recorded, written)
	}
	return recorded, nil
}

// SettleHypothesis moves one hypothesis's state. Setting one aside carries its reason, which the
// schema requires rather than the caller remembering.
func (p *Placements) SettleHypothesis(
	ctx context.Context, organization tenancy.Organization,
	fence investigation.Fence, settled investigation.Hypothesis,
) error {
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}
	tag, err := pool.Exec(ctx, `
		UPDATE investigation_hypothesis
		   SET state = $6, set_aside_reason = $7, updated_at = now()
		  FROM investigation_round
		 WHERE investigation_hypothesis.hypothesis_id = $5
		   AND investigation_hypothesis.round_id      = investigation_round.round_id
		   AND investigation_round.round_id      = $1
		   AND investigation_round.organization  = $2
		   AND investigation_round.lease_session = $3
		   AND investigation_round.lease_epoch   = $4
		   AND investigation_round.outcome      IS NULL`,
		fence.RoundID, organization.String(), fence.LeaseSession, fence.LeaseEpoch,
		settled.ID, int16(settled.State), settled.SetAsideReason)
	if err != nil {
		return fmt.Errorf("settling a hypothesis: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return investigation.ErrLeaseLost
	}
	return nil
}

// RecordRequest appends one proposed read in whatever state validation left it.
//
// A refused request is recorded exactly like a dispatched one, because the record has to show what
// was tried and not only what worked. The schema refuses a refused request that names a job, so
// "nothing was dispatched" is a property of the row rather than of the code path that wrote it.
func (p *Placements) RecordRequest(
	ctx context.Context, organization tenancy.Organization,
	fence investigation.Fence, request investigation.Request,
) (investigation.Request, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return investigation.Request{}, err
	}

	var refusal *int16
	if request.Refusal != 0 {
		value := int16(request.Refusal)
		refusal = &value
	}

	row := pool.QueryRow(ctx, `
		INSERT INTO investigation_request
			(request_id, organization, investigation_id, round_id, ordinal, pass,
			 capability_id, capability_version, arguments, justification, reason,
			 state, refusal, job_id)
		SELECT $5, $2, investigation_round.investigation_id, $1,
		       (SELECT coalesce(max(ordinal), 0) + 1
		          FROM investigation_request existing
		         WHERE existing.round_id = $1),
		       $6, $7, $8, $9, $10, $11, $12, $13, $14`+fencedRound+`
		RETURNING `+requestColumns,
		fence.RoundID, organization.String(), fence.LeaseSession, fence.LeaseEpoch,
		uuid.New(), request.Pass, request.CapabilityID, request.CapabilityVersion,
		request.Arguments, nullableUUID(request.Justification), request.Reason,
		int16(request.State), refusal, nullableUUID(request.JobID))

	written, err := scanRequest(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return investigation.Request{}, investigation.ErrLeaseLost
	}
	if err != nil {
		return investigation.Request{}, fmt.Errorf("recording a capability request: %w", err)
	}
	return written, nil
}

// SettleRequest records what a dispatched read came to.
func (p *Placements) SettleRequest(
	ctx context.Context, organization tenancy.Organization,
	fence investigation.Fence, request investigation.Request,
) error {
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}
	tag, err := pool.Exec(ctx, `
		UPDATE investigation_request
		   SET state = $6, result_bytes = $7, settled_at = now()
		  FROM investigation_round
		 WHERE investigation_request.request_id = $5
		   AND investigation_request.round_id   = investigation_round.round_id
		   AND investigation_round.round_id      = $1
		   AND investigation_round.organization  = $2
		   AND investigation_round.lease_session = $3
		   AND investigation_round.lease_epoch   = $4
		   AND investigation_round.outcome      IS NULL`,
		fence.RoundID, organization.String(), fence.LeaseSession, fence.LeaseEpoch,
		request.ID, int16(request.State), request.ResultBytes)
	if err != nil {
		return fmt.Errorf("settling a capability request: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return investigation.ErrLeaseLost
	}
	return nil
}

// RecordEvidence appends validated items and returns them with their identities and ordinals.
//
// The ordinal is per CASE rather than per round, so a section paginated across a growing case has
// one stable order rather than one per round that a reader has to interleave.
func (p *Placements) RecordEvidence(
	ctx context.Context, organization tenancy.Organization,
	fence investigation.Fence, items []investigation.Item,
) ([]investigation.Item, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return nil, err
	}

	recorded := make([]investigation.Item, 0, len(items))
	for _, item := range items {
		certificate, marshalErr := marshalCertificate(item.Certificate)
		if marshalErr != nil {
			return nil, marshalErr
		}
		row := pool.QueryRow(ctx, `
			INSERT INTO evidence_item
				(evidence_id, organization, investigation_id, round_id, ordinal, request_id,
				 capability_id, capability_version, connection_id, statement, content,
				 absence, trust, certificate, source_observed_at, received_at)
			SELECT $5, $2, investigation_round.investigation_id, $1,
			       (SELECT coalesce(max(ordinal), 0) + 1
			          FROM evidence_item existing
			         WHERE existing.investigation_id = investigation_round.investigation_id),
			       $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16`+fencedRound+`
			RETURNING `+evidenceColumns,
			fence.RoundID, organization.String(), fence.LeaseSession, fence.LeaseEpoch,
			uuid.New(), nullableUUID(item.RequestID), item.CapabilityID, item.CapabilityVersion,
			item.Connection, item.Statement, item.Content, item.Absence, int16(item.Trust),
			certificate, nullableTime(item.SourceObservedAt), orNow(item.ReceivedAt))

		written, scanErr := scanEvidence(row)
		if errors.Is(scanErr, pgx.ErrNoRows) {
			return nil, investigation.ErrLeaseLost
		}
		if scanErr != nil {
			return nil, fmt.Errorf("recording an evidence item: %w", scanErr)
		}
		recorded = append(recorded, written)
	}
	return recorded, nil
}

// RecordGaps appends coverage gaps and returns them with their identities and ordinals.
func (p *Placements) RecordGaps(
	ctx context.Context, organization tenancy.Organization,
	fence investigation.Fence, gaps []investigation.Gap,
) ([]investigation.Gap, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return nil, err
	}

	recorded := make([]investigation.Gap, 0, len(gaps))
	for _, gap := range gaps {
		row := pool.QueryRow(ctx, `
			INSERT INTO coverage_gap
				(gap_id, organization, investigation_id, round_id, ordinal,
				 cause, capability_id, subject, consequence)
			SELECT $5, $2, investigation_round.investigation_id, $1,
			       (SELECT coalesce(max(ordinal), 0) + 1
			          FROM coverage_gap existing
			         WHERE existing.investigation_id = investigation_round.investigation_id),
			       $6, $7, $8, $9`+fencedRound+`
			RETURNING `+gapColumns,
			fence.RoundID, organization.String(), fence.LeaseSession, fence.LeaseEpoch,
			uuid.New(), int16(gap.Cause), gap.CapabilityID, gap.Subject, gap.Consequence)

		written, scanErr := scanGap(row)
		if errors.Is(scanErr, pgx.ErrNoRows) {
			return nil, investigation.ErrLeaseLost
		}
		if scanErr != nil {
			return nil, fmt.Errorf("recording a coverage gap: %w", scanErr)
		}
		recorded = append(recorded, written)
	}
	return recorded, nil
}

// RecordStances records how evidence stands towards hypotheses, including the neutral ones. What
// was weighed and moved nothing is what shows a hypothesis was examined rather than ignored.
func (p *Placements) RecordStances(
	ctx context.Context, organization tenancy.Organization,
	fence investigation.Fence, weighed []investigation.Weighed,
) error {
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}

	for _, stance := range weighed {
		tag, execErr := pool.Exec(ctx, `
			INSERT INTO evidence_stance
				(organization, investigation_id, hypothesis_id, evidence_id, stance, reason)
			SELECT $2, investigation_round.investigation_id, $5, $6, $7, $8`+fencedRound+`
			ON CONFLICT (hypothesis_id, evidence_id) DO UPDATE
			   SET stance = EXCLUDED.stance, reason = EXCLUDED.reason, recorded_at = now()`,
			fence.RoundID, organization.String(), fence.LeaseSession, fence.LeaseEpoch,
			stance.HypothesisID, stance.EvidenceID, int16(stance.Stance), stance.Reason)
		if execErr != nil {
			return fmt.Errorf("recording an evidence stance: %w", execErr)
		}
		if tag.RowsAffected() != 1 {
			return investigation.ErrLeaseLost
		}
	}
	return nil
}

// RecordCoverage records per-capability readiness for this round, replacing what was there.
// Coverage moves as a round proceeds — unavailable becomes checked when a read lands — and a
// coverage section that only ever grew would show both.
func (p *Placements) RecordCoverage(
	ctx context.Context, organization tenancy.Organization,
	fence investigation.Fence, coverage []investigation.Coverage,
) error {
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}

	for _, entry := range coverage {
		tag, execErr := pool.Exec(ctx, `
			INSERT INTO investigation_coverage
				(organization, investigation_id, round_id, capability_id, capability_version,
				 state, reason, evidence_count)
			SELECT $2, investigation_round.investigation_id, $1, $5, $6, $7, $8, $9`+fencedRound+`
			ON CONFLICT (round_id, capability_id, capability_version) DO UPDATE
			   SET state = EXCLUDED.state, reason = EXCLUDED.reason,
			       evidence_count = EXCLUDED.evidence_count`,
			fence.RoundID, organization.String(), fence.LeaseSession, fence.LeaseEpoch,
			entry.CapabilityID, entry.CapabilityVersion, int16(entry.State), entry.Reason,
			entry.Evidence)
		if execErr != nil {
			return fmt.Errorf("recording coverage: %w", execErr)
		}
		if tag.RowsAffected() != 1 {
			return investigation.ErrLeaseLost
		}
	}
	return nil
}

// RecordOutcome writes an outcome, its claims and their citations in ONE transaction, and marks
// the case's previous outcome superseded rather than rewriting it.
//
// One transaction because half of this committing would leave a claim with no citations, which is
// the single shape the truth model may not produce. Superseding rather than replacing because a
// case has a present tense and its rounds are immutable: at 14:32 this was abstained for lack of
// evidence, at 15:10 it became explained, and both are in the record with their attribution
// (ADR-013).
func (p *Placements) RecordOutcome(
	ctx context.Context, organization tenancy.Organization,
	fence investigation.Fence, outcome investigation.Outcome,
) error {
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}

	transaction, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning an outcome: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	var (
		outcomeID       uuid.UUID
		investigationID uuid.UUID
	)
	err = transaction.QueryRow(ctx, `
		INSERT INTO investigation_outcome
			(outcome_id, organization, investigation_id, round_id, round_ordinal,
			 kind, statement, independent_sources)
		SELECT $5, $2, investigation_round.investigation_id, $1, investigation_round.ordinal,
		       $6, $7, $8`+fencedRound+`
		RETURNING outcome_id, investigation_id`,
		fence.RoundID, organization.String(), fence.LeaseSession, fence.LeaseEpoch,
		uuid.New(), int16(outcome.Kind), outcome.Statement, outcome.IndependentSources).
		Scan(&outcomeID, &investigationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return investigation.ErrLeaseLost
	}
	if err != nil {
		return fmt.Errorf("recording an outcome: %w", err)
	}

	// Everything this case had concluded before now is superseded. It stays readable and
	// attributed; what changes is that it is no longer the present tense.
	if _, err = transaction.Exec(ctx, `
		UPDATE investigation_outcome
		   SET superseded = TRUE
		 WHERE investigation_id = $1
		   AND organization     = $2
		   AND outcome_id      <> $3
		   AND NOT superseded`,
		investigationID, organization.String(), outcomeID); err != nil {
		return fmt.Errorf("superseding a previous outcome: %w", err)
	}

	for _, claim := range outcome.Claims {
		claimID := uuid.New()
		if _, err = transaction.Exec(ctx, `
			INSERT INTO outcome_claim
				(claim_id, organization, investigation_id, outcome_id, ordinal, role, statement)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			claimID, organization.String(), investigationID, outcomeID,
			claim.Ordinal, int16(claim.Role), claim.Statement); err != nil {
			return fmt.Errorf("recording a claim: %w", err)
		}
		for _, cited := range claim.Evidence {
			// The foreign key carries the case, so an item from another investigation is refused
			// here rather than discovered by whoever follows the citation.
			if _, err = transaction.Exec(ctx, `
				INSERT INTO claim_citation (organization, investigation_id, claim_id, evidence_id)
				VALUES ($1, $2, $3, $4)`,
				organization.String(), investigationID, claimID, cited); err != nil {
				return fmt.Errorf("recording a citation: %w", err)
			}
		}
	}

	for _, hypothesis := range outcome.UnresolvedHypotheses {
		if _, err = transaction.Exec(ctx, `
			INSERT INTO outcome_unresolved_hypothesis
				(organization, investigation_id, outcome_id, hypothesis_id)
			VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`,
			organization.String(), investigationID, outcomeID, hypothesis); err != nil {
			return fmt.Errorf("recording an unresolved hypothesis: %w", err)
		}
	}
	for _, gap := range outcome.RelevantGaps {
		if _, err = transaction.Exec(ctx, `
			INSERT INTO outcome_relevant_gap (organization, investigation_id, outcome_id, gap_id)
			VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`,
			organization.String(), investigationID, outcomeID, gap); err != nil {
			return fmt.Errorf("recording a relevant gap: %w", err)
		}
	}

	if err = transaction.Commit(ctx); err != nil {
		return fmt.Errorf("committing an outcome: %w", err)
	}
	return nil
}

// Dispatch enqueues one typed read against the case's Connection.
//
// The Connection is a precondition of the insert rather than a value copied in, and the Relay the
// job runs on is taken FROM it rather than trusted from the caller. A read for one customer's
// cluster therefore cannot be sent to an installation sitting in another, however the arguments
// were assembled.
func (p *Placements) Dispatch(
	ctx context.Context, organization tenancy.Organization, sending investigation.Sending,
) (uuid.UUID, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return uuid.Nil, err
	}

	var job uuid.UUID
	err = pool.QueryRow(ctx, `
		INSERT INTO relay_job
			(job_id, organization, connection_id, registration_id,
			 capability_id, capability_version, arguments)
		SELECT $1, $2, connection.connection_id, connection.relay_registration_id, $4, $5, $6
		  FROM connection
		 WHERE connection.connection_id = $3
		   AND connection.organization  = $2
		   AND connection.disabled_at  IS NULL
		   -- 2 evidence, 3 both.
		   AND connection.role         IN (2, 3)
		   AND connection.relay_registration_id IS NOT NULL
		RETURNING job_id`,
		uuid.New(), organization.String(), sending.ConnectionID,
		sending.CapabilityID, sending.CapabilityVersion, sending.Arguments).Scan(&job)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, investigation.ErrConnectionUnusable
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("dispatching a capability read: %w", err)
	}
	return job, nil
}

// Reads reports what dispatched jobs have come to. Terminal states are decided here in Go against
// the declared constants rather than as literals inside the SQL, so the enumeration has one
// authority.
func (p *Placements) Reads(
	ctx context.Context, organization tenancy.Organization, jobs []uuid.UUID,
) ([]investigation.Read, error) {
	if len(jobs) == 0 {
		return nil, nil
	}
	pool, err := p.Pool(organization)
	if err != nil {
		return nil, err
	}

	rows, err := pool.Query(ctx, `
		SELECT job_id, status, result
		  FROM relay_job
		 WHERE organization = $1 AND job_id = ANY($2)`,
		organization.String(), jobs)
	if err != nil {
		return nil, fmt.Errorf("reading dispatched jobs: %w", err)
	}
	defer rows.Close()

	settled := make([]investigation.Read, 0, len(jobs))
	for rows.Next() {
		var (
			read   investigation.Read
			status JobStatus
			result []byte
		)
		if err = rows.Scan(&read.JobID, &status, &result); err != nil {
			return nil, fmt.Errorf("reading a dispatched job: %w", err)
		}
		read.Settled = status != JobPending && status != JobLeased
		read.Succeeded = status == JobSucceeded
		read.Result = result
		settled = append(settled, read)
	}
	return settled, rows.Err()
}

func marshalCertificate(certificate *investigation.Certificate) ([]byte, error) {
	if certificate == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(certificate)
	if err != nil {
		return nil, fmt.Errorf("encoding a completeness certificate: %w", err)
	}
	return encoded, nil
}

// nullableTime renders the zero time as SQL NULL, which is what "the source gave no defensible
// time for this" means in a column the timeline reads.
func nullableTime(at time.Time) *time.Time {
	if at.IsZero() {
		return nil
	}
	return &at
}

func orNow(at time.Time) time.Time {
	if at.IsZero() {
		return time.Now()
	}
	return at
}
