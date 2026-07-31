package storage

import (
	"context"
	"encoding/json"
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
	Status    SignalStatus
	Title     string
	Summary   string
	Labels    map[string]string
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
	ordered := slices.SortedFunc(slices.Values(delivery.Signals), compareSignals)
	for _, signal := range ordered {
		if err = upsertSignal(ctx, transaction, organization, delivery, signal); err != nil {
			return DeliveryOutcome{}, err
		}
	}
	if err = transaction.Commit(ctx); err != nil {
		return DeliveryOutcome{}, fmt.Errorf("committing delivery: %w", err)
	}
	return DeliveryOutcome{Recorded: len(delivery.Signals)}, nil
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
func upsertSignal(
	ctx context.Context, transaction pgx.Tx, organization tenancy.Organization,
	delivery Delivery, signal Signal,
) error {
	labels, err := json.Marshal(signal.Labels)
	if err != nil {
		return fmt.Errorf("encoding signal labels: %w", err)
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
	_, err = transaction.Exec(ctx, `
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
		 WHERE signal.status = 1`,
		uuid.New(), organization.String(), delivery.Connection, delivery.Environment,
		signal.SourceKey, int16(signal.Status),
		signal.Title, signal.Summary, labels, signal.StartedAt, resolvedAt)
	if err != nil {
		return fmt.Errorf("recording signal: %w", err)
	}
	return nil
}
