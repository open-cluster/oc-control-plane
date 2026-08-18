package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// SignalStatus is where an alert has got to. There are two: it is happening, or it stopped.
// Anything richer belongs to the incident an investigation attaches to, not to the alert.
type SignalStatus int16

const (
	SignalFiring SignalStatus = iota + 1
	SignalResolved
)

// Signal is one episode of a normalised alert: this occurrence of it, not the alert in the
// abstract. The distinction is the model's, not a detail — the same alert fires many times.
type Signal struct {
	// SourceKey identifies the ALERT as its source names it, and is stable across episodes.
	SourceKey string
	// GroupingKey is the SOURCE's own notion of what belongs together, and is empty when it
	// supplied none. It is what an IncidentEpisode is keyed on, and it is deliberately never
	// something this platform inferred from the labels below. Empty produces one episode per
	// alert, which is what "the source grouped nothing" honestly means.
	GroupingKey string
	Status      SignalStatus
	Title       string
	Summary     string
	Labels      map[string]string
	// Annotations are the source's own operational pointers — runbook_url,
	// dashboard links — preserved because they are the operator's knowledge already
	// attached to the alert, and untrusted text for their whole life.
	Annotations map[string]string
	// GeneratorURL is where the source says the alert came from, preserved so the
	// alert's own pointer is not thrown away at intake.
	GeneratorURL string
	// StartedAt is when the source says this episode began, and together with SourceKey it
	// is what makes one episode distinguishable from the next.
	StartedAt time.Time
	// ResolvedAt is when the source says it ended, and is zero while it is still firing.
	ResolvedAt time.Time
}

// Delivery is one accepted webhook body and everything in it. The parts travel together
// because they are one fact: this body, through this Integration, carried these signals.
type Delivery struct {
	// Integration is the installation the body arrived through, and the only authority for
	// the tenant everything in it belongs to.
	Integration uuid.UUID
	BodyDigest  []byte
	// Truncated is how many alerts the source says it left out. Non-zero means this record
	// of the moment is incomplete because the sender chose not to send the rest.
	Truncated int
	Signals   []Signal
}

// DeliveryOutcome is what happened to one delivery.
type DeliveryOutcome struct {
	// Duplicate reports that this exact body was already accepted from this source, so
	// nothing was written a second time. It is a success: a source retrying because it
	// never saw a response has done nothing wrong, and the answer must let it stop.
	Duplicate bool
	// Recorded counts the signals this delivery created or updated, and is zero on a
	// duplicate.
	Recorded int
	// EpisodesOpened and EpisodesJoined are the GROUPING outcome. They are reported apart
	// because the ratio is what tells an operator their own alert grouping is doing
	// something.
	EpisodesOpened int
	EpisodesJoined int
}

// DeliveryDisposition is what happened to one delivery attempt, as the history records it.
type DeliveryDisposition int16

const (
	DeliveryAccepted DeliveryDisposition = iota + 1
	DeliveryDuplicate
	DeliveryRejected
)

// Why a delivery was rejected, in the vocabulary the history stores.
const (
	RefusedUnauthenticated = "unauthenticated"
	RefusedMalformed       = "malformed"
	RefusedOversized       = "oversized"
	RefusedIncomplete      = "incomplete"
)

// DeliveryAttempt is one entry in an Integration's delivery history: a duplicate or a
// rejection. An ACCEPTED delivery needs no attempt record — the accepting transaction
// writes its own row, and that row is also the idempotence key.
type DeliveryAttempt struct {
	Integration uuid.UUID
	Disposition DeliveryDisposition
	// Reason says why, for a rejection, and is empty otherwise.
	Reason string
}

// RecordDeliveryAttempt puts a duplicate or a rejection in the history, so a source that
// is delivering and being turned away is distinguishable from a source that has gone
// quiet. Those two call for opposite actions at three in the morning.
func (p *Placements) RecordDeliveryAttempt(
	ctx context.Context, organization tenancy.Organization, attempt DeliveryAttempt,
) error {
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO integration_delivery (delivery_id, org_id, integration_id, outcome, reason)
		VALUES ($1, $2, $3, $4, $5)`,
		uuid.New(), organization.String(), attempt.Integration,
		int16(attempt.Disposition), attempt.Reason); err != nil {
		return fmt.Errorf("recording a delivery attempt: %w", err)
	}
	return nil
}

// RecordDelivery accepts one delivery and everything in it, in one transaction.
//
// Both halves commit together or neither does. A delivery marked accepted whose signals
// were not written would be silently dropped and never retried, because the source would
// be told it succeeded; signals written without the delivery recorded would be applied
// again on the next retry. The unique constraint on the body digest is what resolves two
// concurrent retries — the database decides, rather than a read-then-write both could pass.
func (p *Placements) RecordDelivery(
	ctx context.Context, organization tenancy.Organization, delivery Delivery,
) (DeliveryOutcome, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return DeliveryOutcome{}, err
	}

	transaction, err := pool.Begin(ctx)
	if err != nil {
		return DeliveryOutcome{}, fmt.Errorf("beginning delivery: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	claimed, err := claimDelivery(ctx, transaction, organization, delivery)
	if err != nil {
		return DeliveryOutcome{}, err
	}
	if !claimed {
		return DeliveryOutcome{Duplicate: true}, nil
	}

	// Signals are written in a fixed order. Two deliveries carrying the same alerts in
	// different orders would otherwise take row locks in opposite orders, and Postgres
	// would abort one of them as a deadlock — recoverable, since the source retries, but a
	// self-inflicted failure that costs nothing to avoid.
	var grouping DeliveryOutcome
	ordered := slices.SortedFunc(slices.Values(delivery.Signals), compareSignals)
	for _, signal := range ordered {
		signalID, inserted, upsertErr := upsertSignal(
			ctx, transaction, organization, delivery, signal)
		if upsertErr != nil {
			return DeliveryOutcome{}, upsertErr
		}
		if signalID == uuid.Nil {
			// The guard in upsertSignal matched nothing: a firing redelivered after its own
			// resolution. Nothing changed and nothing should be grouped, because grouping
			// it would reopen an episode this alert has already finished.
			continue
		}
		if inserted {
			// A new episode of this alert. Everything else is an update to a Signal that
			// already has its episode, and moving one would be the history changing.
			opened, groupErr := groupSignal(
				ctx, transaction, organization, delivery, signal, signalID)
			if groupErr != nil {
				return DeliveryOutcome{}, groupErr
			}
			if opened {
				grouping.EpisodesOpened++
			} else {
				grouping.EpisodesJoined++
			}
			continue
		}
		// An update to a Signal already in an episode — most often its resolution, which is
		// what decides whether the episode as a whole has recovered.
		if err = regroupUpdatedSignal(ctx, transaction, signalID); err != nil {
			return DeliveryOutcome{}, err
		}
	}
	if err = transaction.Commit(ctx); err != nil {
		return DeliveryOutcome{}, fmt.Errorf("committing delivery: %w", err)
	}
	grouping.Recorded = len(delivery.Signals)
	return grouping, nil
}

// compareSignals orders two signals by the identity they are written under.
func compareSignals(a, b Signal) int {
	if byKey := strings.Compare(a.SourceKey, b.SourceKey); byKey != 0 {
		return byKey
	}
	return a.StartedAt.Compare(b.StartedAt)
}

// claimDelivery records the accepted delivery, reporting false when this body was already
// accepted. The row it writes is both the history entry and the idempotence key: the
// partial unique index on accepted rows is what makes an at-least-once webhook safe.
func claimDelivery(
	ctx context.Context, transaction pgx.Tx,
	organization tenancy.Organization, delivery Delivery,
) (bool, error) {
	tag, err := transaction.Exec(ctx, `
		INSERT INTO integration_delivery
			(delivery_id, org_id, integration_id, outcome, body_digest, signal_count, truncated)
		VALUES ($1, $2, $3, 1, $4, $5, $6)
		ON CONFLICT (integration_id, body_digest) WHERE outcome = 1 DO NOTHING`,
		uuid.New(), organization.String(), delivery.Integration, delivery.BodyDigest,
		len(delivery.Signals), delivery.Truncated)
	if err != nil {
		return false, fmt.Errorf("recording delivery: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// upsertSignal writes one episode, or updates the episode this source already reported.
//
// Two things are deliberately never rewritten. received_at keeps its original value, so
// when this platform first heard of an episode stays true however many times the source
// repeats it. started_at cannot change at all, because it is half the identity — which is
// what makes a re-fire a new episode rather than an overwrite of the resolved record of
// the last one.
//
// The guard is the point of the WHERE clause. Webhooks are at-least-once AND unordered, so
// a redelivery of the firing can arrive after the resolution that ended it. Updating only
// while the episode is still firing means a late firing cannot resurrect something already
// resolved, and a repeated resolution is a no-op.
func upsertSignal(
	ctx context.Context, transaction pgx.Tx, organization tenancy.Organization,
	delivery Delivery, signal Signal,
) (uuid.UUID, bool, error) {
	labels, err := json.Marshal(signal.Labels)
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("encoding signal labels: %w", err)
	}
	// A nil map must land as an empty document, not JSON null: the column is a set of
	// pointers, and "none" is the empty set.
	annotations := []byte("{}")
	if len(signal.Annotations) > 0 {
		if annotations, err = json.Marshal(signal.Annotations); err != nil {
			return uuid.Nil, false, fmt.Errorf("encoding signal annotations: %w", err)
		}
	}

	var resolvedAt *time.Time
	if signal.Status == SignalResolved {
		resolved := signal.ResolvedAt
		resolvedAt = &resolved
	}

	// xmax is zero on a row this statement INSERTED and non-zero on one it updated. It is
	// how the same statement answers "was this new" without a second query that a
	// concurrent delivery could get a different answer from.
	var (
		signalID uuid.UUID
		inserted bool
	)
	err = transaction.QueryRow(ctx, `
		INSERT INTO signal
			(signal_id, org_id, integration_id, source_key, status,
			 title, summary, labels, annotations, generator_url, started_at, resolved_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (integration_id, source_key, started_at) DO UPDATE
		   SET status        = EXCLUDED.status,
		       title         = EXCLUDED.title,
		       summary       = EXCLUDED.summary,
		       labels        = EXCLUDED.labels,
		       annotations   = EXCLUDED.annotations,
		       generator_url = EXCLUDED.generator_url,
		       resolved_at   = EXCLUDED.resolved_at,
		       updated_at    = now()
		 WHERE signal.status = 1
		RETURNING signal_id, xmax = 0`,
		uuid.New(), organization.String(), delivery.Integration,
		signal.SourceKey, int16(signal.Status),
		signal.Title, signal.Summary, labels, annotations, signal.GeneratorURL,
		signal.StartedAt, resolvedAt).
		Scan(&signalID, &inserted)
	if errors.Is(err, pgx.ErrNoRows) {
		// The guard refused the update: this episode of the alert is already resolved and a
		// firing has arrived late. Nothing was written, which is the point of the guard.
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("recording signal: %w", err)
	}
	return signalID, inserted, nil
}

// regroupUpdatedSignal brings the episode of an already-grouped Signal back in line with
// it. The Signal itself does not move — an update never changes which episode a Signal
// belongs to. What is recomputed is the episode's own state, most importantly whether
// every Signal in it has now stopped firing.
func regroupUpdatedSignal(
	ctx context.Context, transaction pgx.Tx, signalID uuid.UUID,
) error {
	var episodeID *uuid.UUID
	if err := transaction.QueryRow(ctx,
		`SELECT episode_id FROM signal WHERE signal_id = $1`, signalID).Scan(&episodeID); err != nil {
		return fmt.Errorf("reading a signal's episode: %w", err)
	}
	if episodeID == nil {
		return nil
	}
	return refreshEpisode(ctx, transaction, *episodeID)
}
