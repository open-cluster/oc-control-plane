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

	"github.com/open-cluster/oc-control-plane/internal/alertevent"
)

// Delivery is one accepted webhook body and everything in it. The parts travel together
// because they are one fact: this body, through this Integration, carried these alertEvents.
type Delivery struct {
	// Integration is the installation the body arrived through, and the only authority for
	// the tenant everything in it belongs to.
	Integration uuid.UUID
	RequestID   string
	alertevent.AlertDelivery
}

// ErrDeliveryIdentityConflict means a provider reused one lifecycle identity for
// different normalized content. Retrying cannot make that payload safe to accept.
var ErrDeliveryIdentityConflict = errors.New("delivery identity conflicts with accepted content")

// DeliveryOutcome is what happened to one delivery.
type DeliveryOutcome struct {
	// Duplicate reports that this exact body was already accepted from this source, so
	// nothing was written a second time. It is a success: a source retrying because it
	// never saw a response has done nothing wrong, and the answer must let it stop.
	Duplicate bool
	// Recorded counts the alertEvents this delivery created or updated, and is zero on a
	// duplicate.
	Recorded int
	// IncidentsOpened and IncidentsJoined are the GROUPING outcome. They are reported apart
	// because the ratio is what tells an operator their own alert grouping is doing
	// something.
	IncidentsOpened int
	IncidentsJoined int
}

// RecordDelivery accepts one delivery and everything in it, in one transaction.
//
// Delivery facts, Incident changes, and automatic Investigations commit together.
// The unique provider identity and lifecycle key makes concurrent retries idempotent.
func (p *Database) RecordDelivery(
	ctx context.Context, organization uuid.UUID, delivery Delivery, policy AlertAdmissionPolicy,
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

	_, claimed, err := claimDelivery(ctx, transaction, organization, delivery)
	if err != nil {
		return DeliveryOutcome{}, err
	}
	if !claimed {
		return DeliveryOutcome{Duplicate: true}, nil
	}

	// AlertEvents are written in a fixed order. Two deliveries carrying the same alerts in
	// different orders would otherwise take row locks in opposite orders, and Postgres
	// would abort one of them as a deadlock — recoverable, since the source retries, but a
	// self-inflicted failure that costs nothing to avoid.
	var grouping DeliveryOutcome
	var openedIncidents []uuid.UUID
	ordered := slices.SortedFunc(slices.Values(delivery.AlertEvents), compareAlertEvents)
	for _, alertEvent := range ordered {
		alertEventID, inserted, upsertErr := upsertAlertEvent(
			ctx, transaction, organization, delivery, alertEvent)
		if upsertErr != nil {
			return DeliveryOutcome{}, upsertErr
		}
		if alertEventID == uuid.Nil {
			// The guard in upsertAlertEvent matched nothing: a firing redelivered after its own
			// resolution. Nothing changed and nothing should be grouped, because grouping
			// it would reopen an incident this alert has already finished.
			continue
		}
		if inserted {
			if alertEvent.Status == alertevent.AlertEventResolved {
				// A resolution with no matching firing remains visible as a source fact, but
				// cannot create the incident it claims already existed.
				continue
			}
			// A new incident of this alert. Everything else is an update to a AlertEvent that
			// already has its incident, and moving one would be the history changing.
			incidentID, opened, groupErr := groupAlertEvent(
				ctx, transaction, organization, delivery, alertEvent, alertEventID)
			if groupErr != nil {
				return DeliveryOutcome{}, groupErr
			}
			if opened {
				grouping.IncidentsOpened++
				openedIncidents = append(openedIncidents, incidentID)
			} else {
				grouping.IncidentsJoined++
			}
			continue
		}
		// An update to a AlertEvent already in an incident — most often its resolution, which is
		// what decides whether the incident as a whole has recovered.
		if err = regroupUpdatedAlertEvent(ctx, transaction, organization, alertEventID); err != nil {
			return DeliveryOutcome{}, err
		}
	}
	if err := openAlertInvestigations(ctx, transaction, organization, openedIncidents, policy); err != nil {
		return DeliveryOutcome{}, err
	}
	if err = transaction.Commit(ctx); err != nil {
		return DeliveryOutcome{}, fmt.Errorf("committing delivery: %w", err)
	}
	grouping.Recorded = len(delivery.AlertEvents)
	return grouping, nil
}

// compareAlertEvents orders two alertEvents by the identity they are written under.
func compareAlertEvents(a, b alertevent.AlertEvent) int {
	if byKey := strings.Compare(a.SourceKey, b.SourceKey); byKey != 0 {
		return byKey
	}
	return a.StartedAt.Compare(b.StartedAt)
}

// claimDelivery records accepted content, reporting false when the provider identity and
// lifecycle phase were already accepted. Their unique key makes retries idempotent; the
// digest detects a provider identity reused for different content.
func claimDelivery(
	ctx context.Context, transaction pgx.Tx,
	organization uuid.UUID, delivery Delivery,
) (uuid.UUID, bool, error) {
	deliveryID := uuid.New()
	providerIdentity := delivery.ProviderIdentity
	if providerIdentity == "" {
		providerIdentity = fmt.Sprintf("%x", delivery.ContentDigest)
	}
	tag, err := transaction.Exec(ctx, `
		INSERT INTO webhook_delivery
			(delivery_id, org_id, integration_id, content_digest, provider_identity,
			 lifecycle_phase, request_id, truncated)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (integration_id, provider_identity, lifecycle_phase)
		DO NOTHING`,
		deliveryID, organization, delivery.Integration, delivery.ContentDigest,
		providerIdentity, delivery.LifecyclePhase, delivery.RequestID,
		delivery.Truncated)
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("recording delivery: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return deliveryID, true, nil
	}
	var acceptedDigest []byte
	if err := transaction.QueryRow(ctx, `
		SELECT content_digest FROM webhook_delivery
		 WHERE org_id = $1 AND integration_id = $2
		   AND provider_identity = $3 AND lifecycle_phase = $4`,
		organization, delivery.Integration, providerIdentity,
		delivery.LifecyclePhase).Scan(&acceptedDigest); err != nil {
		return uuid.Nil, false, fmt.Errorf("reading accepted delivery identity: %w", err)
	}
	if !slices.Equal(acceptedDigest, delivery.ContentDigest) {
		return uuid.Nil, false, ErrDeliveryIdentityConflict
	}
	return deliveryID, false, nil
}

// upsertAlertEvent writes one incident, or updates the incident this source already reported.
//
// Two things are deliberately never rewritten. received_at keeps its original value, so
// when this platform first heard of an incident stays true however many times the source
// repeats it. started_at cannot change at all, because it is half the identity — which is
// what makes a re-fire a new incident rather than an overwrite of the resolved record of
// the last one.
//
// The guard is the point of the WHERE clause. Webhooks are at-least-once AND unordered, so
// a redelivery of the firing can arrive after the resolution that ended it. Updating only
// while the incident is still firing means a late firing cannot resurrect something already
// resolved, and a repeated resolution is a no-op.
func upsertAlertEvent(
	ctx context.Context, transaction pgx.Tx, organization uuid.UUID,
	delivery Delivery, alertEvent alertevent.AlertEvent,
) (uuid.UUID, bool, error) {
	labels, err := json.Marshal(alertEvent.Labels)
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("encoding alert_event labels: %w", err)
	}
	// A nil map must land as an empty document, not JSON null: the column is a set of
	// pointers, and "none" is the empty set.
	annotations := []byte("{}")
	if len(alertEvent.Annotations) > 0 {
		if annotations, err = json.Marshal(alertEvent.Annotations); err != nil {
			return uuid.Nil, false, fmt.Errorf("encoding alert_event annotations: %w", err)
		}
	}

	var resolvedAt *time.Time
	if alertEvent.Status == alertevent.AlertEventResolved {
		resolved := alertEvent.ResolvedAt
		resolvedAt = &resolved
	}

	// xmax is zero on a row this statement INSERTED and non-zero on one it updated. It is
	// how the same statement answers "was this new" without a second query that a
	// concurrent delivery could get a different answer from.
	var (
		alertEventID uuid.UUID
		inserted     bool
	)
	err = transaction.QueryRow(ctx, `
		INSERT INTO alert_event
			(alert_event_id, org_id, integration_id, source_key, status,
			 title, summary, labels, annotations, generator_url, started_at, resolved_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, now())
		ON CONFLICT (integration_id, source_key, started_at) DO UPDATE
		   SET status        = EXCLUDED.status,
		       title         = EXCLUDED.title,
		       summary       = EXCLUDED.summary,
		       labels        = EXCLUDED.labels,
		       annotations   = EXCLUDED.annotations,
		       generator_url = EXCLUDED.generator_url,
		       resolved_at   = EXCLUDED.resolved_at,
		       updated_at    = now()
		 WHERE alert_event.status = 1
		RETURNING alert_event_id, xmax = 0`,
		uuid.New(), organization, delivery.Integration,
		alertEvent.SourceKey, int16(alertEvent.Status),
		alertEvent.Title, alertEvent.Summary, labels, annotations, alertEvent.GeneratorURL,
		alertEvent.StartedAt, resolvedAt).
		Scan(&alertEventID, &inserted)
	if errors.Is(err, pgx.ErrNoRows) {
		// The guard refused the update: this incident of the alert is already resolved and a
		// firing has arrived late. Nothing was written, which is the point of the guard.
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("recording alert_event: %w", err)
	}
	return alertEventID, inserted, nil
}

// regroupUpdatedAlertEvent brings the incident of an already-grouped AlertEvent back in line with
// it. The AlertEvent itself does not move — an update never changes which incident a AlertEvent
// belongs to. What is recomputed is the incident's own state, most importantly whether
// every AlertEvent in it has now stopped firing.
func regroupUpdatedAlertEvent(
	ctx context.Context, transaction pgx.Tx,
	organization uuid.UUID, alertEventID uuid.UUID,
) error {
	var incidentID *uuid.UUID
	if err := transaction.QueryRow(ctx,
		`SELECT incident_id FROM alert_event WHERE alert_event_id = $1 AND org_id = $2`,
		alertEventID, organization).Scan(&incidentID); err != nil {
		return fmt.Errorf("reading a alert_event's incident: %w", err)
	}
	if incidentID == nil {
		return nil
	}
	return refreshIncident(ctx, transaction, organization, *incidentID)
}
