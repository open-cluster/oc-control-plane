package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// Reading a case: one small summary a client polls, and separate paginated sections it fetches
// only when they are opened.
//
// Every section read below carries the case version it represents, and takes it FROM THE SAME
// SNAPSHOT as the rows. Reading the version before or after the section would stamp a page with a
// number that does not describe it, and a client that could not tell a stale section from a
// current one is a client that will eventually mix them.

// The shapes of the case pack's tables, written down once each so a column added to one read and
// forgotten in another is a compile error rather than a field that is silently always zero.
const (
	hypothesisColumns = `
		hypothesis_id, investigation_id, round_id, ordinal, statement, falsifies,
		state, set_aside_reason, pass, proposed_at, updated_at`

	outcomeColumns = `
		outcome_id, round_id, round_ordinal, kind, statement, independent_sources,
		explains_hypothesis_id, superseded, reached_at`

	requestColumns = `
		request_id, investigation_id, round_id, ordinal, pass, capability_id, capability_version,
		arguments, justification, reason, state, refusal, job_id, result_bytes,
		proposed_at, settled_at`

	evidenceColumns = `
		evidence_id, investigation_id, round_id, ordinal, request_id, capability_id,
		capability_version, connection_id, statement, content, absence, trust, certificate,
		source_observed_at, received_at`

	// evidenceListColumns is evidenceColumns with the content left out, so a listing is not the
	// size of its contents. The empty literal keeps one scanner serving both reads rather than
	// producing a second one that could drift.
	evidenceListColumns = `
		evidence_id, investigation_id, round_id, ordinal, request_id, capability_id,
		capability_version, connection_id, statement, '' AS content, absence, trust, certificate,
		source_observed_at, received_at`

	gapColumns = `
		gap_id, investigation_id, round_id, ordinal, cause, capability_id, subject,
		consequence, recorded_at`
)

// snapshot runs a read and the case's version in ONE repeatable-read transaction, so the version a
// page is stamped with is the version that page describes.
//
// It also resolves the case, which is what makes every section read tenant-scoped by construction:
// a request naming one organization's identity and another's investigation finds no case here and
// never reaches the section query at all.
// sectionCursor resolves a section's resume point, reporting the capability's own refusal. A
// cursor that did not come from a previous page is an error rather than a silent restart: showing
// a reader the first page again would let them believe they had seen the last.
func sectionCursor(cursor string) (int, error) {
	ordinal, err := decodeOrdinalCursor(cursor)
	if err != nil {
		return 0, investigation.ErrBadCursor
	}
	return ordinal, nil
}

func (p *Placements) snapshot(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID,
	read func(pgx.Tx) error,
) (int64, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return 0, err
	}

	transaction, err := pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return 0, fmt.Errorf("beginning a case read: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	var version int64
	err = transaction.QueryRow(ctx, `
		SELECT case_version FROM investigation
		 WHERE investigation_id = $1 AND organization = $2`,
		id, organization.String()).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, investigation.ErrUnknown
	}
	if err != nil {
		return 0, fmt.Errorf("reading a case version: %w", err)
	}

	if err = read(transaction); err != nil {
		return 0, err
	}
	return version, nil
}

// pagedSection runs one paginated section read: resolve the page, take the rows and the case
// version in ONE snapshot, and stamp the page with the version it describes.
//
// It is generic because every section is the same read with a different table under it. The five
// copies it replaces differed only in their query, their scanner and the noun in their error text,
// and five copies of a paging loop is five places for the off-by-one at the page boundary to be
// fixed four times.
//
// The cursor is the ORDINAL, which is assigned once and never moves. That is what lets a client
// page through a case that is still growing without a row it has already seen appearing again.
func pagedSection[T any](
	ctx context.Context, p *Placements, organization tenancy.Organization, id uuid.UUID,
	page investigation.Page, subject string,
	query func(transaction pgx.Tx, after, limit int) (pgx.Rows, error),
	scan func(scanned) (T, error),
	ordinal func(T) int,
) (investigation.Section[T], error) {
	limit := pageLimit(page.Limit)
	after, err := sectionCursor(page.After)
	if err != nil {
		return investigation.Section[T]{}, err
	}

	section := investigation.Section[T]{Items: make([]T, 0, limit)}
	version, err := p.snapshot(ctx, organization, id, func(transaction pgx.Tx) error {
		// One more row than the page is asked for, so whether a next page exists is answered by
		// what came back rather than by a second count that could disagree with it.
		rows, queryErr := query(transaction, after, limit+1)
		if queryErr != nil {
			return fmt.Errorf("listing %s: %w", subject, queryErr)
		}
		defer rows.Close()

		for rows.Next() {
			item, scanErr := scan(rows)
			if scanErr != nil {
				return fmt.Errorf("reading %s: %w", subject, scanErr)
			}
			if len(section.Items) == limit {
				section.Next = encodeOrdinalCursor(ordinal(section.Items[limit-1]))
				break
			}
			section.Items = append(section.Items, item)
		}
		return rows.Err()
	})
	if err != nil {
		return investigation.Section[T]{}, err
	}
	section.CaseVersion = version
	return section, nil
}

// The ordinal each section pages by, named so a caller below reads as one line.
func itemOrdinal(item investigation.Item) int                   { return item.Ordinal }
func gapOrdinal(gap investigation.Gap) int                      { return gap.Ordinal }
func requestOrdinal(request investigation.Request) int          { return request.Ordinal }
func hypothesisOrdinal(hypothesis investigation.Hypothesis) int { return hypothesis.Ordinal }

// InvestigationEvidence returns a page of a case's EvidenceItems, without their content.
func (p *Placements) InvestigationEvidence(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID,
	filter investigation.EvidenceFilter, page investigation.Page,
) (investigation.Section[investigation.Item], error) {
	return pagedSection(ctx, p, organization, id, page, "evidence",
		func(transaction pgx.Tx, after, limit int) (pgx.Rows, error) {
			return transaction.Query(ctx, `
				SELECT `+evidenceListColumns+`
				  FROM evidence_item
				 WHERE evidence_item.investigation_id = $1
				   AND evidence_item.organization     = $2
				   AND evidence_item.ordinal          > $3
				   AND ($5 = '' OR evidence_item.capability_id = $5)
				   AND ($6::uuid IS NULL OR evidence_item.connection_id = $6)
				   AND ($7::smallint IS NULL OR EXISTS (
				        SELECT 1 FROM evidence_stance
				         WHERE evidence_stance.evidence_id = evidence_item.evidence_id
				           AND evidence_stance.stance      = $7))
				 ORDER BY evidence_item.ordinal
				 LIMIT $4`,
				id, organization.String(), after, limit,
				filter.CapabilityID, nullableUUID(filter.Source), nullableStance(filter.Stance))
		}, scanEvidence, itemOrdinal)
}

// EvidenceItem reads one item WITH its content, bounded by the column that stores it.
//
// It is a separate read from the listing on purpose: an item's content is up to the bound and a
// listing that carried it would be the size of its contents. The bound applies here as well as on
// the write path, which is the same defect from the other direction.
func (p *Placements) EvidenceItem(
	ctx context.Context, organization tenancy.Organization, id, evidenceID uuid.UUID,
) (investigation.Item, int64, error) {
	var item investigation.Item
	version, err := p.snapshot(ctx, organization, id, func(transaction pgx.Tx) error {
		row := transaction.QueryRow(ctx, `
			SELECT `+evidenceColumns+`
			  FROM evidence_item
			 WHERE evidence_id      = $1
			   AND investigation_id = $2
			   AND organization     = $3`,
			evidenceID, id, organization.String())
		found, scanErr := scanEvidence(row)
		if errors.Is(scanErr, pgx.ErrNoRows) {
			return investigation.ErrUnknown
		}
		if scanErr != nil {
			return fmt.Errorf("reading an evidence item: %w", scanErr)
		}
		item = found
		return nil
	})
	if err != nil {
		return investigation.Item{}, 0, err
	}
	return item, version, nil
}

// InvestigationTimeline returns the items carrying a defensible source time, in source order.
//
// It is derived rather than authored, and it deliberately leaves out items with no such time —
// those are listed beside it by the evidence read. Placing them would require inventing an
// ordering, and ordering is the whole reason a timeline is read.
func (p *Placements) InvestigationTimeline(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID,
	page investigation.Page,
) (investigation.Section[investigation.Item], error) {
	return pagedSection(ctx, p, organization, id, page, "a timeline",
		func(transaction pgx.Tx, after, limit int) (pgx.Rows, error) {
			// Ordered by source time, then by ordinal so two observations sharing a timestamp keep
			// one order across pages. The cursor is the ordinal, which never moves; a cursor on the
			// timestamp would reshuffle under a client the moment two items shared one.
			return transaction.Query(ctx, `
				SELECT `+evidenceListColumns+`
				  FROM evidence_item
				 WHERE investigation_id    = $1
				   AND organization        = $2
				   AND source_observed_at IS NOT NULL
				   AND ordinal             > $3
				 ORDER BY source_observed_at, ordinal
				 LIMIT $4`,
				id, organization.String(), after, limit)
		}, scanEvidence, itemOrdinal)
}

// InvestigationHypotheses returns a page of a case's hypotheses, ordered by the round that
// proposed them and then by the planner's ordinal.
func (p *Placements) InvestigationHypotheses(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID,
	page investigation.Page,
) (investigation.Section[investigation.Hypothesis], error) {
	return pagedSection(ctx, p, organization, id, page, "hypotheses",
		func(transaction pgx.Tx, after, limit int) (pgx.Rows, error) {
			return transaction.Query(ctx, `
				SELECT `+hypothesisColumns+`
				  FROM investigation_hypothesis
				 WHERE investigation_id = $1
				   AND organization     = $2
				   AND ordinal          > $3
				 ORDER BY round_id, ordinal
				 LIMIT $4`,
				id, organization.String(), after, limit)
		}, scanHypothesis, hypothesisOrdinal)
}

// InvestigationGaps returns a page of a case's coverage gaps.
func (p *Placements) InvestigationGaps(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID,
	page investigation.Page,
) (investigation.Section[investigation.Gap], error) {
	return pagedSection(ctx, p, organization, id, page, "coverage gaps",
		func(transaction pgx.Tx, after, limit int) (pgx.Rows, error) {
			return transaction.Query(ctx, `
				SELECT `+gapColumns+`
				  FROM coverage_gap
				 WHERE investigation_id = $1
				   AND organization     = $2
				   AND ordinal          > $3
				 ORDER BY ordinal
				 LIMIT $4`,
				id, organization.String(), after, limit)
		}, scanGap, gapOrdinal)
}

// InvestigationActivity returns a page of the reads a case asked for, with the hypothesis that
// justified each.
//
// Requests that returned nothing useful are in here with everything else. Evidence selection is
// scored independently of the conclusion (ADR-009), which is only possible if what was asked and
// why survives into the read model — including the asks that produced nothing.
func (p *Placements) InvestigationActivity(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID,
	page investigation.Page,
) (investigation.Section[investigation.Request], error) {
	return pagedSection(ctx, p, organization, id, page, "activity",
		func(transaction pgx.Tx, after, limit int) (pgx.Rows, error) {
			return transaction.Query(ctx, `
				SELECT `+requestColumns+`
				  FROM investigation_request
				 WHERE investigation_id = $1
				   AND organization     = $2
				   AND ordinal          > $3
				 ORDER BY round_id, ordinal
				 LIMIT $4`,
				id, organization.String(), after, limit)
		}, scanRequest, requestOrdinal)
}

// InvestigationCoverage returns coverage per typed capability for the case's current round.
func (p *Placements) InvestigationCoverage(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID,
) (investigation.Section[investigation.Coverage], error) {
	var section investigation.Section[investigation.Coverage]
	version, err := p.snapshot(ctx, organization, id, func(transaction pgx.Tx) error {
		coverage, readErr := readCoverage(ctx, transaction, organization, id, uuid.Nil)
		if readErr != nil {
			return readErr
		}
		section.Items = coverage
		return nil
	})
	if err != nil {
		return investigation.Section[investigation.Coverage]{}, err
	}
	section.CaseVersion = version
	return section, nil
}
