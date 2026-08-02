package storage

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// The case pack: one round's immutable closed-world record, and the round-scoped readers that
// compose it.
//
// Every read here is by ROUND and none of them consults live state. That is what replays a round
// with no access to a cluster, a relay or the configuration of the day — and it is why they take a
// transaction rather than opening one, so a caller that needs several gets one snapshot rather
// than a set of reads describing different moments.

// CasePack reads one round's closed-world record.
//
// Every read is by round, and none of them consults live state: this is what replays a round with
// no access to a cluster, a relay or the configuration of the day. A conclusion that surprises
// someone months later is examinable from this and nothing else.
func (p *Placements) CasePack(
	ctx context.Context, organization tenancy.Organization, roundID uuid.UUID,
) (investigation.CasePack, error) {
	round, err := p.Round(ctx, organization, roundID)
	if err != nil {
		return investigation.CasePack{}, err
	}
	found, err := p.Investigation(ctx, organization, round.InvestigationID)
	if err != nil {
		return investigation.CasePack{}, err
	}

	pool, err := p.Pool(organization)
	if err != nil {
		return investigation.CasePack{}, err
	}
	transaction, err := pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return investigation.CasePack{}, fmt.Errorf("beginning a case pack read: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	pack := investigation.CasePack{Investigation: found, Round: round}
	if pack.Hypotheses, err = readHypotheses(ctx, transaction, organization, roundID); err != nil {
		return investigation.CasePack{}, err
	}
	if pack.Requests, err = readRequests(ctx, transaction, organization, roundID); err != nil {
		return investigation.CasePack{}, err
	}
	if pack.Evidence, err = readEvidence(ctx, transaction, organization, roundID); err != nil {
		return investigation.CasePack{}, err
	}
	if pack.Gaps, err = readGaps(ctx, transaction, organization, roundID); err != nil {
		return investigation.CasePack{}, err
	}
	if pack.Stances, err = readStances(ctx, transaction, organization, roundID); err != nil {
		return investigation.CasePack{}, err
	}
	if pack.Coverage, err = readCoverage(
		ctx, transaction, organization, found.ID, roundID); err != nil {
		return investigation.CasePack{}, err
	}
	outcomes, err := readOutcomes(ctx, transaction, organization, found.ID, roundID)
	if err != nil {
		return investigation.CasePack{}, err
	}
	if len(outcomes) == 1 {
		pack.Outcome = &outcomes[0]
	}
	return pack, nil
}

// The round-scoped readers below are shared by the case pack and by server-side assembly. They
// take a transaction rather than opening one, so a caller that needs several of them gets one
// snapshot rather than a set of reads describing different moments.

func readHypotheses(
	ctx context.Context, on pgx.Tx, organization tenancy.Organization, roundID uuid.UUID,
) ([]investigation.Hypothesis, error) {
	rows, err := on.Query(ctx, `
		SELECT `+hypothesisColumns+`
		  FROM investigation_hypothesis
		 WHERE round_id = $1 AND organization = $2
		 ORDER BY ordinal`, roundID, organization.String())
	if err != nil {
		return nil, fmt.Errorf("reading hypotheses: %w", err)
	}
	defer rows.Close()

	var found []investigation.Hypothesis
	for rows.Next() {
		hypothesis, scanErr := scanHypothesis(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("reading a hypothesis: %w", scanErr)
		}
		found = append(found, hypothesis)
	}
	return found, rows.Err()
}

func readRequests(
	ctx context.Context, on pgx.Tx, organization tenancy.Organization, roundID uuid.UUID,
) ([]investigation.Request, error) {
	rows, err := on.Query(ctx, `
		SELECT `+requestColumns+`
		  FROM investigation_request
		 WHERE round_id = $1 AND organization = $2
		 ORDER BY ordinal`, roundID, organization.String())
	if err != nil {
		return nil, fmt.Errorf("reading capability requests: %w", err)
	}
	defer rows.Close()

	var found []investigation.Request
	for rows.Next() {
		request, scanErr := scanRequest(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("reading a capability request: %w", scanErr)
		}
		found = append(found, request)
	}
	return found, rows.Err()
}

func readEvidence(
	ctx context.Context, on pgx.Tx, organization tenancy.Organization, roundID uuid.UUID,
) ([]investigation.Item, error) {
	rows, err := on.Query(ctx, `
		SELECT `+evidenceColumns+`
		  FROM evidence_item
		 WHERE round_id = $1 AND organization = $2
		 ORDER BY ordinal`, roundID, organization.String())
	if err != nil {
		return nil, fmt.Errorf("reading evidence: %w", err)
	}
	defer rows.Close()

	var found []investigation.Item
	for rows.Next() {
		item, scanErr := scanEvidence(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("reading an evidence item: %w", scanErr)
		}
		found = append(found, item)
	}
	return found, rows.Err()
}

func readGaps(
	ctx context.Context, on pgx.Tx, organization tenancy.Organization, roundID uuid.UUID,
) ([]investigation.Gap, error) {
	rows, err := on.Query(ctx, `
		SELECT `+gapColumns+`
		  FROM coverage_gap
		 WHERE round_id = $1 AND organization = $2
		 ORDER BY ordinal`, roundID, organization.String())
	if err != nil {
		return nil, fmt.Errorf("reading coverage gaps: %w", err)
	}
	defer rows.Close()

	var found []investigation.Gap
	for rows.Next() {
		gap, scanErr := scanGap(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("reading a coverage gap: %w", scanErr)
		}
		found = append(found, gap)
	}
	return found, rows.Err()
}

func readStances(
	ctx context.Context, on pgx.Tx, organization tenancy.Organization, roundID uuid.UUID,
) ([]investigation.Weighed, error) {
	rows, err := on.Query(ctx, `
		SELECT evidence_stance.hypothesis_id, evidence_stance.evidence_id,
		       evidence_stance.stance, evidence_stance.reason, evidence_stance.recorded_at
		  FROM evidence_stance
		  JOIN investigation_hypothesis
		    ON investigation_hypothesis.hypothesis_id = evidence_stance.hypothesis_id
		 WHERE investigation_hypothesis.round_id = $1
		   AND evidence_stance.organization      = $2
		 ORDER BY investigation_hypothesis.ordinal, evidence_stance.evidence_id`,
		roundID, organization.String())
	if err != nil {
		return nil, fmt.Errorf("reading evidence stances: %w", err)
	}
	defer rows.Close()

	var found []investigation.Weighed
	for rows.Next() {
		var (
			weighed investigation.Weighed
			stance  int16
		)
		if err = rows.Scan(&weighed.HypothesisID, &weighed.EvidenceID, &stance,
			&weighed.Reason, &weighed.RecordedAt); err != nil {
			return nil, fmt.Errorf("reading an evidence stance: %w", err)
		}
		weighed.Stance = investigation.Stance(stance)
		found = append(found, weighed)
	}
	return found, rows.Err()
}

// readCoverage reads per-capability readiness. A zero round reads the case's latest round, which is
// what a client asking "what is covered" means; naming one reads that round, which is what the
// case pack means.
func readCoverage(
	ctx context.Context, on pgx.Tx, organization tenancy.Organization,
	investigationID, roundID uuid.UUID,
) ([]investigation.Coverage, error) {
	rows, err := on.Query(ctx, `
		SELECT capability_id, capability_version, state, reason, evidence_count
		  FROM investigation_coverage
		 WHERE investigation_id = $1
		   AND organization     = $2
		   AND round_id = COALESCE($3::uuid, (
		        SELECT round_id FROM investigation_round
		         WHERE investigation_round.investigation_id = $1
		         ORDER BY ordinal DESC LIMIT 1))
		 ORDER BY capability_id, capability_version`,
		investigationID, organization.String(), nullableUUID(roundID))
	if err != nil {
		return nil, fmt.Errorf("reading coverage: %w", err)
	}
	defer rows.Close()

	var found []investigation.Coverage
	for rows.Next() {
		var (
			coverage investigation.Coverage
			state    int16
		)
		if err = rows.Scan(&coverage.CapabilityID, &coverage.CapabilityVersion, &state,
			&coverage.Reason, &coverage.Evidence); err != nil {
			return nil, fmt.Errorf("reading a coverage entry: %w", err)
		}
		coverage.State = investigation.CoverageState(state)
		found = append(found, coverage)
	}
	return found, rows.Err()
}

// readOutcomes reads a case's outcomes newest first, or one round's when a round is named. A
// superseded outcome comes back with the rest: it stays readable, attributed and ordered rather
// than being rewritten.
func readOutcomes(
	ctx context.Context, on pgx.Tx, organization tenancy.Organization,
	investigationID, roundID uuid.UUID,
) ([]investigation.Outcome, error) {
	rows, err := on.Query(ctx, `
		SELECT `+outcomeColumns+`
		  FROM investigation_outcome
		 WHERE investigation_id = $1
		   AND organization     = $2
		   AND ($3::uuid IS NULL OR round_id = $3)
		 ORDER BY round_ordinal DESC, reached_at DESC`,
		investigationID, organization.String(), nullableUUID(roundID))
	if err != nil {
		return nil, fmt.Errorf("reading outcomes: %w", err)
	}

	var found []investigation.Outcome
	for rows.Next() {
		outcome, scanErr := scanOutcome(rows)
		if scanErr != nil {
			rows.Close()
			return nil, fmt.Errorf("reading an outcome: %w", scanErr)
		}
		found = append(found, outcome)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("reading outcomes: %w", err)
	}

	for index := range found {
		if err = attachClaims(ctx, on, organization, &found[index]); err != nil {
			return nil, err
		}
	}
	return found, nil
}

// attachClaims fills in one outcome's cited claims and the hypotheses and gaps it named. The
// citations come back as identifiers rather than as prose, because flattening a claim to prose is
// what turns an inspectable artifact into an assertable one.
func attachClaims(
	ctx context.Context, on pgx.Tx, organization tenancy.Organization,
	outcome *investigation.Outcome,
) error {
	rows, err := on.Query(ctx, `
		SELECT outcome_claim.claim_id, outcome_claim.ordinal, outcome_claim.role,
		       outcome_claim.statement,
		       coalesce(array_agg(claim_citation.evidence_id)
		                FILTER (WHERE claim_citation.evidence_id IS NOT NULL), '{}')
		  FROM outcome_claim
		  LEFT JOIN claim_citation ON claim_citation.claim_id = outcome_claim.claim_id
		 WHERE outcome_claim.outcome_id   = $1
		   AND outcome_claim.organization = $2
		 GROUP BY outcome_claim.claim_id, outcome_claim.ordinal, outcome_claim.role,
		          outcome_claim.statement
		 ORDER BY outcome_claim.ordinal`,
		outcome.ID, organization.String())
	if err != nil {
		return fmt.Errorf("reading claims: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			claim investigation.Claim
			role  int16
		)
		if err = rows.Scan(&claim.ID, &claim.Ordinal, &role, &claim.Statement,
			&claim.Evidence); err != nil {
			return fmt.Errorf("reading a claim: %w", err)
		}
		claim.Role = investigation.ClaimRole(role)
		outcome.Claims = append(outcome.Claims, claim)
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("reading claims: %w", err)
	}

	if outcome.UnresolvedHypotheses, err = readIdentifiers(ctx, on, `
		SELECT hypothesis_id FROM outcome_unresolved_hypothesis
		 WHERE outcome_id = $1 AND organization = $2 ORDER BY hypothesis_id`,
		outcome.ID, organization); err != nil {
		return err
	}
	outcome.RelevantGaps, err = readIdentifiers(ctx, on, `
		SELECT gap_id FROM outcome_relevant_gap
		 WHERE outcome_id = $1 AND organization = $2 ORDER BY gap_id`,
		outcome.ID, organization)
	return err
}

func readIdentifiers(
	ctx context.Context, on pgx.Tx, query string, subject uuid.UUID,
	organization tenancy.Organization,
) ([]uuid.UUID, error) {
	rows, err := on.Query(ctx, query, subject, organization.String())
	if err != nil {
		return nil, fmt.Errorf("reading identifiers: %w", err)
	}
	defer rows.Close()

	var found []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err = rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("reading an identifier: %w", err)
		}
		found = append(found, id)
	}

	return found, rows.Err()
}
