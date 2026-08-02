package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// The two reads that are one row each, and the one that is the whole case.
//
// The summary is the only thing a client polls, so its cost must not grow with the case: it is a
// fixed number of statements, and every count in it is a column maintained by trigger rather than
// an aggregate computed on the way out. The list is one statement regardless of how many rows come
// back, which is the answer to the defect the frozen .NET frontend audit recorded — cursor lists
// with no totals, and an incident list fanning out one request per row.

// maxAssembledEvidence bounds server-side assembly. A case larger than this is refused by name
// rather than answered with a response nobody bounded: an export is a document, and a document
// whose size is decided by how long an incident ran is a download that fails in a browser.
const maxAssembledEvidence = 500

// InvestigationSummary reads the top of one case.
func (p *Placements) InvestigationSummary(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID,
) (investigation.Summary, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return investigation.Summary{}, err
	}

	transaction, err := pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return investigation.Summary{}, fmt.Errorf("beginning a summary read: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	summary, err := readSummary(ctx, transaction, organization, id)
	if err != nil {
		return investigation.Summary{}, err
	}
	return summary, nil
}

func readSummary(
	ctx context.Context, on pgx.Tx, organization tenancy.Organization, id uuid.UUID,
) (investigation.Summary, error) {
	var summary investigation.Summary
	row := on.QueryRow(ctx, `
		SELECT `+investigationColumns+`,
		       round_count, evidence_count, timeline_count, gap_count, hypothesis_count,
		       request_count, outcome_count, spend_tokens, spend_micro_cents, spend_millis
		  FROM investigation
		 WHERE investigation_id = $1 AND organization = $2`,
		id, organization.String())

	found, counts, spend, err := scanSummaryRow(row, organization.String())
	if errors.Is(err, pgx.ErrNoRows) {
		return investigation.Summary{}, investigation.ErrUnknown
	}
	if err != nil {
		return investigation.Summary{}, fmt.Errorf("reading an investigation summary: %w", err)
	}
	summary.Investigation, summary.Counts, summary.Spend = found, counts, spend

	// The current round carries the brief, the resolved controls and the component versions, which
	// is what makes the top of the case renderable without a second round trip.
	roundRow := on.QueryRow(ctx, `
		SELECT `+roundColumns+`
		  FROM investigation_round
		 WHERE investigation_id = $1 AND organization = $2
		 ORDER BY ordinal DESC LIMIT 1`,
		id, organization.String())
	round, err := scanRound(roundRow)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// A case with no round yet. Everything above is still true and is what a client renders.
	case err != nil:
		return investigation.Summary{}, fmt.Errorf("reading a current round: %w", err)
	default:
		summary.CurrentRound = round
	}

	outcomes, err := readCurrentOutcome(ctx, on, organization, id)
	if err != nil {
		return investigation.Summary{}, err
	}
	if len(outcomes) == 1 {
		summary.Outcome = &outcomes[0]
	}
	return summary, nil
}

// readCurrentOutcome reads the case's present tense: the one outcome nothing has superseded.
func readCurrentOutcome(
	ctx context.Context, on pgx.Tx, organization tenancy.Organization, id uuid.UUID,
) ([]investigation.Outcome, error) {
	rows, err := on.Query(ctx, `
		SELECT `+outcomeColumns+`
		  FROM investigation_outcome
		 WHERE investigation_id = $1 AND organization = $2 AND NOT superseded
		 ORDER BY round_ordinal DESC LIMIT 1`,
		id, organization.String())
	if err != nil {
		return nil, fmt.Errorf("reading the current outcome: %w", err)
	}

	var current []investigation.Outcome
	for rows.Next() {
		outcome, scanErr := scanOutcome(rows)
		if scanErr != nil {
			rows.Close()
			return nil, fmt.Errorf("reading the current outcome: %w", scanErr)
		}
		current = append(current, outcome)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the current outcome: %w", err)
	}

	for index := range current {
		if err = attachClaims(ctx, on, organization, &current[index]); err != nil {
			return nil, err
		}
	}
	return current, nil
}

// ListInvestigations returns a page of cases, ordered by lifecycle state and then recency.
//
// The ordering is defensible in a review rather than merely convenient: a case still being
// investigated is what an on-call engineer wants at the top, and among cases in one state the most
// recently changed is the one being watched. Attributed severity is available as a secondary
// signal and deliberately does not outrank either — a severity a customer's own alerting chose is
// not this product's judgement of what matters.
//
// It is ONE statement whatever the row count. The outcome each row shows is joined laterally
// rather than fetched per row, which is the shape the frozen .NET audit recorded the absence of.
func (p *Placements) ListInvestigations(
	ctx context.Context, organization tenancy.Organization,
	filter investigation.ListFilter, page investigation.Page,
) (investigation.List, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return investigation.List{}, err
	}
	limit := pageLimit(page.Limit)
	after, afterID, err := decodeCursor(page.After)
	if err != nil {
		return investigation.List{}, investigation.ErrBadCursor
	}

	rows, err := pool.Query(ctx, `
		SELECT `+investigationColumns+`,
		       investigation.round_count, investigation.evidence_count,
		       investigation.timeline_count, investigation.gap_count,
		       investigation.hypothesis_count, investigation.request_count,
		       investigation.outcome_count, investigation.spend_tokens,
		       investigation.spend_micro_cents, investigation.spend_millis,
		       investigation.severity, investigation.severity_source,
		       current.kind, current.statement
		  FROM investigation
		  LEFT JOIN LATERAL (
		       SELECT kind, statement
		         FROM investigation_outcome
		        WHERE investigation_outcome.investigation_id = investigation.investigation_id
		          AND NOT investigation_outcome.superseded
		        ORDER BY round_ordinal DESC LIMIT 1) AS current ON TRUE
		 WHERE investigation.organization = $1
		   AND ($4::uuid IS NULL OR investigation.environment_id = $4)
		   AND (NOT $5 OR investigation.lifecycle = ANY($6))
		   AND ($2::timestamptz IS NULL
		        OR (investigation.updated_at, investigation.investigation_id) < ($2, $3::uuid))
		 ORDER BY investigation.lifecycle, investigation.updated_at DESC,
		          investigation.investigation_id DESC
		 LIMIT $7`,
		organization.String(), after, afterID, nullableUUID(filter.Environment),
		filter.Running, runningLifecycles(), limit+1)
	if err != nil {
		return investigation.List{}, fmt.Errorf("listing investigations: %w", err)
	}
	defer rows.Close()

	list := investigation.List{Rows: make([]investigation.Row, 0, limit)}
	for rows.Next() {
		row, scanErr := scanInvestigationRow(rows, organization.String())
		if scanErr != nil {
			return investigation.List{}, fmt.Errorf("reading an investigation: %w", scanErr)
		}
		if len(list.Rows) == limit {
			last := list.Rows[limit-1].Investigation
			list.Next = encodeCursor(last.UpdatedAt, last.ID)
			break
		}
		list.Rows = append(list.Rows, row)
	}
	if err = rows.Err(); err != nil {
		return investigation.List{}, fmt.Errorf("listing investigations: %w", err)
	}
	return list, nil
}

// runningLifecycles is the set a worker currently holds. Built from the declared constants rather
// than written as literals inside the SQL, so the enumeration has one authority.
func runningLifecycles() []int16 {
	return []int16{
		int16(investigation.LifecycleBriefing),
		int16(investigation.LifecycleReasoning),
		int16(investigation.LifecycleGathering),
	}
}

// AssembleCaseFile builds the complete case at a pinned version, server-side, in one snapshot.
//
// pinned names the version the caller means. Zero pins whatever the case is at now. A pinned
// version the case has already passed is REFUSED rather than answered from the current state:
// answering would hand back a document stamped with one version and containing another, which is
// the failure a pinned export exists to prevent.
func (p *Placements) AssembleCaseFile(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID, pinned int64,
) (investigation.CaseFile, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return investigation.CaseFile{}, err
	}

	transaction, err := pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return investigation.CaseFile{}, fmt.Errorf("beginning an assembly: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	summary, err := readSummary(ctx, transaction, organization, id)
	if err != nil {
		return investigation.CaseFile{}, err
	}
	if pinned != 0 && pinned != summary.Investigation.CaseVersion {
		return investigation.CaseFile{}, fmt.Errorf("%w: asked for %d, the case is at %d",
			investigation.ErrCaseMoved, pinned, summary.Investigation.CaseVersion)
	}
	if summary.Counts.Evidence > maxAssembledEvidence {
		return investigation.CaseFile{}, fmt.Errorf("%w: %d evidence items, the ceiling is %d",
			investigation.ErrCaseTooLarge, summary.Counts.Evidence, maxAssembledEvidence)
	}

	file := investigation.CaseFile{
		Investigation: summary.Investigation,
		CaseVersion:   summary.Investigation.CaseVersion,
	}
	if file.Rounds, err = readRoundsIn(ctx, transaction, organization, id); err != nil {
		return investigation.CaseFile{}, err
	}
	if file.Hypotheses, err = readCaseHypotheses(ctx, transaction, organization, id); err != nil {
		return investigation.CaseFile{}, err
	}
	if file.Evidence, err = readCaseEvidence(ctx, transaction, organization, id); err != nil {
		return investigation.CaseFile{}, err
	}
	if file.Gaps, err = readCaseGaps(ctx, transaction, organization, id); err != nil {
		return investigation.CaseFile{}, err
	}
	if file.Requests, err = readCaseRequests(ctx, transaction, organization, id); err != nil {
		return investigation.CaseFile{}, err
	}
	if file.Stances, err = readCaseStances(ctx, transaction, organization, id); err != nil {
		return investigation.CaseFile{}, err
	}
	if file.Coverage, err = readCoverage(ctx, transaction, organization, id, uuid.Nil); err != nil {
		return investigation.CaseFile{}, err
	}
	if file.Outcomes, err = readOutcomes(
		ctx, transaction, organization, id, uuid.Nil); err != nil {
		return investigation.CaseFile{}, err
	}

	// The timeline is derived from the evidence already read rather than queried again, so an
	// assembled case file cannot contain a timeline describing a different moment from its
	// evidence.
	for _, item := range file.Evidence {
		if item.OnTimeline() {
			file.Timeline = append(file.Timeline, item)
		}
	}
	sortTimeline(file.Timeline)
	return file, nil
}

// sortTimeline orders by source time, then by ordinal so two observations sharing a timestamp keep
// one order. It is the same ordering the paginated timeline read uses; assembling with a different
// one would make the export disagree with the page it came from.
func sortTimeline(items []investigation.Item) {
	for outer := 1; outer < len(items); outer++ {
		for inner := outer; inner > 0; inner-- {
			if !items[inner].SourceObservedAt.Before(items[inner-1].SourceObservedAt) {
				break
			}
			items[inner], items[inner-1] = items[inner-1], items[inner]
		}
	}
}

// The case-wide readers below mirror the round-scoped ones in investigation_read.go. They are
// separate rather than parameterised because the WHERE clause is the whole difference and a shared
// one built by string concatenation is how a tenant filter goes missing from one caller.

func readRoundsIn(
	ctx context.Context, on pgx.Tx, organization tenancy.Organization, id uuid.UUID,
) ([]investigation.Round, error) {
	rows, err := on.Query(ctx, `
		SELECT `+roundColumns+`
		  FROM investigation_round
		 WHERE investigation_id = $1 AND organization = $2
		 ORDER BY ordinal`, id, organization.String())
	if err != nil {
		return nil, fmt.Errorf("assembling rounds: %w", err)
	}
	defer rows.Close()

	var found []investigation.Round
	for rows.Next() {
		round, scanErr := scanRound(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("assembling a round: %w", scanErr)
		}
		found = append(found, round)
	}
	return found, rows.Err()
}

func readCaseHypotheses(
	ctx context.Context, on pgx.Tx, organization tenancy.Organization, id uuid.UUID,
) ([]investigation.Hypothesis, error) {
	rows, err := on.Query(ctx, `
		SELECT `+hypothesisColumns+`
		  FROM investigation_hypothesis
		 WHERE investigation_id = $1 AND organization = $2
		 ORDER BY round_id, ordinal`, id, organization.String())
	if err != nil {
		return nil, fmt.Errorf("assembling hypotheses: %w", err)
	}
	defer rows.Close()

	var found []investigation.Hypothesis
	for rows.Next() {
		hypothesis, scanErr := scanHypothesis(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("assembling a hypothesis: %w", scanErr)
		}
		found = append(found, hypothesis)
	}
	return found, rows.Err()
}

func readCaseEvidence(
	ctx context.Context, on pgx.Tx, organization tenancy.Organization, id uuid.UUID,
) ([]investigation.Item, error) {
	rows, err := on.Query(ctx, `
		SELECT `+evidenceColumns+`
		  FROM evidence_item
		 WHERE investigation_id = $1 AND organization = $2
		 ORDER BY ordinal`, id, organization.String())
	if err != nil {
		return nil, fmt.Errorf("assembling evidence: %w", err)
	}
	defer rows.Close()

	var found []investigation.Item
	for rows.Next() {
		item, scanErr := scanEvidence(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("assembling an evidence item: %w", scanErr)
		}
		found = append(found, item)
	}
	return found, rows.Err()
}

func readCaseGaps(
	ctx context.Context, on pgx.Tx, organization tenancy.Organization, id uuid.UUID,
) ([]investigation.Gap, error) {
	rows, err := on.Query(ctx, `
		SELECT `+gapColumns+`
		  FROM coverage_gap
		 WHERE investigation_id = $1 AND organization = $2
		 ORDER BY ordinal`, id, organization.String())
	if err != nil {
		return nil, fmt.Errorf("assembling coverage gaps: %w", err)
	}
	defer rows.Close()

	var found []investigation.Gap
	for rows.Next() {
		gap, scanErr := scanGap(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("assembling a coverage gap: %w", scanErr)
		}
		found = append(found, gap)
	}
	return found, rows.Err()
}

func readCaseRequests(
	ctx context.Context, on pgx.Tx, organization tenancy.Organization, id uuid.UUID,
) ([]investigation.Request, error) {
	rows, err := on.Query(ctx, `
		SELECT `+requestColumns+`
		  FROM investigation_request
		 WHERE investigation_id = $1 AND organization = $2
		 ORDER BY round_id, ordinal`, id, organization.String())
	if err != nil {
		return nil, fmt.Errorf("assembling capability requests: %w", err)
	}
	defer rows.Close()

	var found []investigation.Request
	for rows.Next() {
		request, scanErr := scanRequest(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("assembling a capability request: %w", scanErr)
		}
		found = append(found, request)
	}
	return found, rows.Err()
}

func readCaseStances(
	ctx context.Context, on pgx.Tx, organization tenancy.Organization, id uuid.UUID,
) ([]investigation.Weighed, error) {
	rows, err := on.Query(ctx, `
		SELECT hypothesis_id, evidence_id, stance, reason, recorded_at
		  FROM evidence_stance
		 WHERE investigation_id = $1 AND organization = $2
		 ORDER BY hypothesis_id, evidence_id`, id, organization.String())
	if err != nil {
		return nil, fmt.Errorf("assembling evidence stances: %w", err)
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
			return nil, fmt.Errorf("assembling an evidence stance: %w", err)
		}
		weighed.Stance = investigation.Stance(stance)
		found = append(found, weighed)
	}
	return found, rows.Err()
}

func scanSummaryRow(
	row scanned, organization string,
) (investigation.Investigation, investigation.SectionCounts, investigation.Spend, error) {
	found, counts, spend, _, _, _, _, err := scanCaseRow(row, organization, false)
	return found, counts, spend, err
}

func scanInvestigationRow(row scanned, organization string) (investigation.Row, error) {
	found, counts, spend, severity, source, kind, statement, err :=
		scanCaseRow(row, organization, true)
	if err != nil {
		return investigation.Row{}, err
	}
	return investigation.Row{
		Investigation:    found,
		Counts:           counts,
		Spend:            spend,
		Severity:         severity,
		SeveritySource:   source,
		OutcomeKind:      kind,
		OutcomeStatement: statement,
	}, nil
}

// scanCaseRow maps a case row with its counters, and optionally the listing's extra columns. One
// function serves both so a counter added to the summary and forgotten in the list is impossible.
func scanCaseRow(row scanned, organization string, listing bool) (
	investigation.Investigation, investigation.SectionCounts, investigation.Spend,
	string, string, investigation.OutcomeKind, string, error,
) {
	var (
		found      = investigation.Investigation{Organization: organization}
		counts     investigation.SectionCounts
		spend      investigation.Spend
		episode    *string
		kind       int16
		trigger    int16
		lifecycle  int16
		terminalAt *time.Time
		millis     int64
		severity   *string
		source     *string
		outcome    *int16
		statement  *string
	)

	destinations := []any{
		&found.ID, &found.Environment, &found.Scope.Connection, &episode,
		&found.Scope.Namespace, &kind, &found.Scope.WorkloadName,
		&found.Window.Start, &found.Window.End,
		&trigger, &found.Trigger.RequestedBy, &found.Trigger.At,
		&lifecycle, &found.CaseVersion, &found.CurrentRound,
		&found.CreatedAt, &found.UpdatedAt, &terminalAt,
		&counts.Rounds, &counts.Evidence, &counts.Timeline, &counts.Gaps,
		&counts.Hypotheses, &counts.Requests, &counts.Outcomes,
		&spend.Tokens, &spend.MicroCents, &millis,
	}
	if listing {
		destinations = append(destinations, &severity, &source, &outcome, &statement)
	}
	if err := row.Scan(destinations...); err != nil {
		return investigation.Investigation{}, investigation.SectionCounts{}, investigation.Spend{},
			"", "", 0, "", err
	}

	if episode != nil {
		found.EpisodeKey = *episode
	}
	if terminalAt != nil {
		found.TerminalAt = *terminalAt
	}
	found.Scope.WorkloadKind = investigation.WorkloadKind(kind)
	found.Trigger.Kind = investigation.TriggerKind(trigger)
	found.Lifecycle = investigation.Lifecycle(lifecycle)
	spend.Duration = time.Duration(millis) * time.Millisecond

	return found, counts, spend,
		orEmpty(severity), orEmpty(source), outcomeKindOf(outcome), orEmpty(statement), nil
}

func orEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func outcomeKindOf(value *int16) investigation.OutcomeKind {
	if value == nil {
		return 0
	}
	return investigation.OutcomeKind(*value)
}
