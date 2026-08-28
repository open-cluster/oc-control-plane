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

	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
)

// AlertEventStatus is where an alert has got to. There are two: it is happening, or it stopped.
// Anything richer belongs to the incident an investigation attaches to, not to the alert.
type AlertEventStatus int16

const (
	AlertEventFiring AlertEventStatus = iota + 1
	AlertEventResolved
)

// AlertEvent is one incident of a normalised alert: this occurrence of it, not the alert in the
// abstract. The distinction is the model's, not a detail — the same alert fires many times.
type AlertEvent struct {
	// SourceKey identifies the ALERT as its source names it, and is stable across incidents.
	SourceKey string
	// GroupingKey is the SOURCE's own notion of what belongs together, and is empty when it
	// supplied none. It is what an Incident is keyed on, and it is deliberately never
	// something this platform inferred from the labels below. Empty produces one incident per
	// alert, which is what "the source grouped nothing" honestly means.
	GroupingKey string
	Status      AlertEventStatus
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
	// StartedAt is when the source says this incident began, and together with SourceKey it
	// is what makes one incident distinguishable from the next.
	StartedAt time.Time
	// ResolvedAt is when the source says it ended, and is zero while it is still firing.
	ResolvedAt time.Time
}

// Delivery is one accepted webhook body and everything in it. The parts travel together
// because they are one fact: this body, through this Integration, carried these alertEvents.
type Delivery struct {
	// Integration is the installation the body arrived through, and the only authority for
	// the tenant everything in it belongs to.
	Integration      uuid.UUID
	ProviderIdentity string
	LifecyclePhase   string
	RequestID        string
	BodyDigest       []byte
	// Truncated is how many alerts the source says it left out. Non-zero means this record
	// of the moment is incomplete because the sender chose not to send the rest.
	Truncated   int
	AlertEvents []AlertEvent
}

// ErrDeliveryIdentityConflict means a provider reused one lifecycle identity for
// different normalized content. Retrying cannot make that payload safe to accept.
var ErrDeliveryIdentityConflict = errors.New("delivery identity conflicts with accepted content")

// NormalizedDelivery is the provider-owned meaning of one authenticated webhook body.
// Identity and digest are computed after validation so semantically identical encodings
// remain one delivery.
type NormalizedDelivery struct {
	ProviderIdentity string
	LifecyclePhase   string
	ContentDigest    []byte
	Truncated        int
	AlertEvents      []AlertEvent
}

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
func (p *Database) RecordDeliveryAttempt(
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
// Both halves commit together or neither does. A delivery marked accepted whose alertEvents
// were not written would be silently dropped and never retried, because the source would
// be told it succeeded; alertEvents written without the delivery recorded would be applied
// again on the next retry. The unique constraint on the body digest is what resolves two
// concurrent retries — the database decides, rather than a read-then-write both could pass.
func (p *Database) RecordDelivery(
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

	deliveryID, claimed, err := claimDelivery(ctx, transaction, organization, delivery)
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
			if alertEvent.Status == AlertEventResolved {
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
				if alertEvent.Status == AlertEventFiring {
					if err := enqueueWebhookWork(ctx, transaction, organization, WebhookWorkAlert,
						deliveryID, delivery.Integration, incidentID, uuid.Nil, 0); err != nil {
						return DeliveryOutcome{}, err
					}
				}
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
	if err = transaction.Commit(ctx); err != nil {
		return DeliveryOutcome{}, fmt.Errorf("committing delivery: %w", err)
	}
	grouping.Recorded = len(delivery.AlertEvents)
	return grouping, nil
}

// compareAlertEvents orders two alertEvents by the identity they are written under.
func compareAlertEvents(a, b AlertEvent) int {
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
) (uuid.UUID, bool, error) {
	deliveryID := uuid.New()
	providerIdentity := delivery.ProviderIdentity
	if providerIdentity == "" {
		providerIdentity = fmt.Sprintf("%x", delivery.BodyDigest)
	}
	tag, err := transaction.Exec(ctx, `
		INSERT INTO integration_delivery
			(delivery_id, org_id, integration_id, outcome, body_digest, provider_identity,
			 lifecycle_phase, request_id, alert_event_count, truncated)
		VALUES ($1, $2, $3, 1, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (integration_id, provider_identity, lifecycle_phase)
		WHERE outcome = 1 DO NOTHING`,
		deliveryID, organization.String(), delivery.Integration, delivery.BodyDigest,
		providerIdentity, delivery.LifecyclePhase, delivery.RequestID,
		len(delivery.AlertEvents), delivery.Truncated)
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("recording delivery: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return deliveryID, true, nil
	}
	var acceptedDigest []byte
	if err := transaction.QueryRow(ctx, `
		SELECT body_digest FROM integration_delivery
		 WHERE org_id = $1 AND integration_id = $2
		   AND provider_identity = $3 AND lifecycle_phase = $4 AND outcome = 1`,
		organization.String(), delivery.Integration, providerIdentity,
		delivery.LifecyclePhase).Scan(&acceptedDigest); err != nil {
		return uuid.Nil, false, fmt.Errorf("reading accepted delivery identity: %w", err)
	}
	if !slices.Equal(acceptedDigest, delivery.BodyDigest) {
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
	ctx context.Context, transaction pgx.Tx, organization tenancy.Organization,
	delivery Delivery, alertEvent AlertEvent,
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
	if alertEvent.Status == AlertEventResolved {
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
		 WHERE alert_event.status = 1
		RETURNING alert_event_id, xmax = 0`,
		uuid.New(), organization.String(), delivery.Integration,
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
	organization tenancy.Organization, alertEventID uuid.UUID,
) error {
	var incidentID *uuid.UUID
	if err := transaction.QueryRow(ctx,
		`SELECT incident_id FROM alert_event WHERE alert_event_id = $1 AND org_id = $2`,
		alertEventID, organization.String()).Scan(&incidentID); err != nil {
		return fmt.Errorf("reading a alert_event's incident: %w", err)
	}
	if incidentID == nil {
		return nil
	}
	return refreshIncident(ctx, transaction, organization, *incidentID)
}
