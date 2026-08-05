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
	// something this platform inferred from the labels below: deciding from those that two alerts
	// concern one thing is canonical resource identity, which does not exist here. Empty produces
	// one episode per alert, which is what "the source grouped nothing" honestly means.
	GroupingKey string
	Status      SignalStatus
	Title       string
	Summary     string
	Labels      map[string]string
	// StartedAt is when the source says this episode began, and together with SourceKey it is
	// what makes one episode distinguishable from the next.
	StartedAt time.Time
	// ResolvedAt is when the source says it ended, and is zero while it is still firing.
	ResolvedAt time.Time
}

// Delivery is one accepted webhook body and everything in it. The parts travel together
// because they are one fact: this body, through this Connection, carried these signals.
type Delivery struct {
	// Connection is the trigger Connection the body arrived through, and the only authority
	// for what follows.
	Connection uuid.UUID
	// Environment is inherited from that Connection and never declared by the caller. It is
	// persisted with each Signal rather than joined, so a Connection that later moves does not
	// silently rewrite the scope everything it already delivered was gathered under.
	Environment uuid.UUID
	BodyDigest  []byte
	// Truncated is how many alerts the source says it left out. Non-zero means this record of
	// the moment is incomplete because the sender chose not to send the rest.
	Truncated int
	Signals   []Signal
}

// DeliveryOutcome is what happened to one delivery.
type DeliveryOutcome struct {
	// Duplicate reports that this exact body was already accepted from this source, so nothing
	// was written a second time. It is a success: a source retrying because it never saw a
	// response has done nothing wrong, and the answer must let it stop.
	Duplicate bool
	// Recorded counts the signals this delivery created or updated, and is zero on a duplicate.
	Recorded int
	// EpisodesOpened and EpisodesJoined are the GROUPING outcome: how many of the new Signals
	// started an operational episode and how many landed in one that was already open. They are
	// reported apart because the ratio is what tells an operator their own alert grouping is doing
	// something — a source where every alert opens its own episode is one whose group_by is not
	// what its author believes.
	EpisodesOpened int
	EpisodesJoined int
}

// RecordDelivery accepts one delivery and everything in it, in one transaction.
//
// Both halves commit together or neither does. A delivery marked accepted whose signals were
// not written would be silently dropped and never retried, because the source would be told
// it succeeded; signals written without the delivery recorded would be applied again on the
// next retry. The unique constraint on the body digest is what resolves two concurrent
// retries — the database decides, rather than a read-then-write both could pass.
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
	// different orders would otherwise take row locks in opposite orders, and Postgres would
	// abort one of them as a deadlock — recoverable, since the source retries, but a
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
			// resolution. Nothing changed and nothing should be grouped, because grouping it would
			// reopen an episode this alert has already finished.
			continue
		}
		if inserted {
			// A new episode of this alert. Everything else is an update to a Signal that already
			// has its episode, and moving one would be the history changing.
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
		// An update to a Signal already in an episode — most often its resolution, which is what
		// decides whether the episode as a whole has recovered.
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

// claimDelivery records the delivery, reporting false when this body was already accepted.
func claimDelivery(
	ctx context.Context, transaction pgx.Tx,
	organization tenancy.Organization, delivery Delivery,
) (bool, error) {
	tag, err := transaction.Exec(ctx, `
		INSERT INTO signal_delivery
			(delivery_id, organization, connection_id, body_digest, signal_count, truncated)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (connection_id, body_digest) DO NOTHING`,
		uuid.New(), organization.String(), delivery.Connection, delivery.BodyDigest,
		len(delivery.Signals), delivery.Truncated)
	if err != nil {
		return false, fmt.Errorf("recording delivery: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// upsertSignal writes one episode, or updates the episode this source already reported.
//
// Two things are deliberately never rewritten. received_at keeps its original value, so when
// this platform first heard of an episode stays true however many times the source repeats it.
// started_at cannot change at all, because it is half the identity — which is what makes a
// re-fire a new episode rather than an overwrite of the resolved record of the last one.
//
// The guard is the point of the WHERE clause. Webhooks are at-least-once AND unordered, so a
// redelivery of the firing can arrive after the resolution that ended it. Updating only while
// the episode is still firing means a late firing cannot resurrect something already resolved,
// and a repeated resolution is a no-op. The only transition this model has is firing to
// resolved, so a state guard says that more directly than comparing timestamps would.
// It reports the Signal's identity and whether the row was newly INSERTED, and a nil identity when
// the guard matched nothing at all. The three answers call for three different things from
// grouping, and collapsing them is how a late redelivery reopens an episode that already closed.
func upsertSignal(
	ctx context.Context, transaction pgx.Tx, organization tenancy.Organization,
	delivery Delivery, signal Signal,
) (uuid.UUID, bool, error) {
	labels, err := json.Marshal(signal.Labels)
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("encoding signal labels: %w", err)
	}

	var resolvedAt *time.Time
	if signal.Status == SignalResolved {
		resolved := signal.ResolvedAt
		resolvedAt = &resolved
	}

	// The environment is written from the delivery, which took it from the Connection. It is
	// deliberately NOT in the update list: a Signal keeps the scope it was gathered under even
	// if the Connection is later moved, because rewriting the scope of past evidence is exactly
	// what an environment boundary exists to prevent.
	//
	// xmax is zero on a row this statement INSERTED and non-zero on one it updated. It is how the
	// same statement answers "was this new" without a second query that a concurrent delivery
	// could get a different answer from.
	var (
		signalID uuid.UUID
		inserted bool
	)
	err = transaction.QueryRow(ctx, `
		INSERT INTO signal
			(signal_id, organization, connection_id, environment_id, source_key, status,
			 title, summary, labels, started_at, resolved_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (connection_id, source_key, started_at) DO UPDATE
		   SET status      = EXCLUDED.status,
		       title       = EXCLUDED.title,
		       summary     = EXCLUDED.summary,
		       labels      = EXCLUDED.labels,
		       resolved_at = EXCLUDED.resolved_at,
		       updated_at  = now()
		 WHERE signal.status = 1
		RETURNING signal_id, xmax = 0`,
		uuid.New(), organization.String(), delivery.Connection, delivery.Environment,
		signal.SourceKey, int16(signal.Status),
		signal.Title, signal.Summary, labels, signal.StartedAt, resolvedAt).
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

// regroupUpdatedSignal brings the episode of an already-grouped Signal back in line with it.
//
// The Signal itself does not move — an update never changes which episode a Signal belongs to, and
// one that did would be the history changing. What is recomputed is the episode's own state, most
// importantly whether every Signal in it has now stopped firing.
//
// A Signal recorded before this existed has no episode, and this leaves it alone rather than
// inventing one: the grouping identity was the source's and that delivery is gone.
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
