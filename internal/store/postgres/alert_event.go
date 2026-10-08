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

type Delivery struct {
	Integration uuid.UUID
	alertevent.AlertDelivery
}

var ErrDeliveryIdentityConflict = errors.New("delivery identity conflicts with accepted content")

type DeliveryOutcome struct {
	Duplicate       bool
	Recorded        int
	IncidentsOpened int
	IncidentsJoined int
}

func (p *Database) RecordDelivery(
	ctx context.Context, organization uuid.UUID, delivery Delivery, policy AlertAdmissionPolicy,
) (DeliveryOutcome, error) {
	// Delivery facts, Incident changes, and automatic Investigations commit together. The
	// provider identity and lifecycle key make concurrent retries idempotent.
	pool, err := p.poolForOrganization(organization)
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

	var grouping DeliveryOutcome
	var openedIncidents []uuid.UUID
	// Fixed lock order prevents concurrent deliveries of the same alerts from deadlocking.
	ordered := slices.SortedFunc(slices.Values(delivery.AlertEvents), compareAlertEvents)
	for _, alertEvent := range ordered {
		alertEventID, inserted, upsertErr := upsertAlertEvent(
			ctx, transaction, organization, delivery, alertEvent)
		if upsertErr != nil {
			return DeliveryOutcome{}, upsertErr
		}
		if alertEventID == uuid.Nil {
			continue
		}
		if inserted {
			if alertEvent.Status == alertevent.AlertEventResolved {
				continue
			}
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

func compareAlertEvents(a, b alertevent.AlertEvent) int {
	if byKey := strings.Compare(a.SourceKey, b.SourceKey); byKey != 0 {
		return byKey
	}
	return a.StartedAt.Compare(b.StartedAt)
}

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
			 lifecycle_phase, truncated)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (integration_id, provider_identity, lifecycle_phase)
		DO NOTHING`,
		deliveryID, organization, delivery.Integration, delivery.ContentDigest,
		providerIdentity, delivery.LifecyclePhase, delivery.Truncated)
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

func upsertAlertEvent(
	ctx context.Context, transaction pgx.Tx, organization uuid.UUID,
	delivery Delivery, alertEvent alertevent.AlertEvent,
) (uuid.UUID, bool, error) {
	// Webhooks are unordered: a late firing must not overwrite a resolution and reopen its Incident.
	labels, err := json.Marshal(alertEvent.Labels)
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("encoding alert_event labels: %w", err)
	}
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
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("recording alert_event: %w", err)
	}
	return alertEventID, inserted, nil
}

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

type AlertAdmissionPolicy struct {
	WindowLead     time.Duration
	MaximumPending int
	AgentAvailable bool
}

type AlertCapacityError struct{}

func (AlertCapacityError) Error() string { return "pending Investigation capacity exhausted" }

type AlertBatchTooLargeError struct{}

func (AlertBatchTooLargeError) Error() string {
	return "alert batch exceeds pending Investigation limit"
}

func openAlertInvestigations(
	ctx context.Context, tx pgx.Tx, organization uuid.UUID,
	incidents []uuid.UUID, policy AlertAdmissionPolicy,
) error {
	if !policy.AgentAvailable || len(incidents) == 0 {
		return nil
	}
	if policy.MaximumPending > 0 && len(incidents) > policy.MaximumPending {
		return AlertBatchTooLargeError{}
	}
	if err := reserveWaitingInvestigations(ctx, tx, organization, policy.MaximumPending, len(incidents)); err != nil {
		if errors.Is(err, ErrInvestigationCapacity) {
			return AlertCapacityError{}
		}
		return err
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO investigation
			(investigation_id, org_id, incident_id, subject, window_from,
			 window_until, created_by, automatic_incident)
		SELECT gen_random_uuid(), org_id, incident_id, title,
		       first_seen_at - ($3 * interval '1 microsecond'), last_seen_at, 'webhook', true
		  FROM incident
		 WHERE org_id = $1 AND incident_id = ANY($2::uuid[])`,
		organization, incidents, policy.WindowLead.Microseconds())
	if err != nil {
		return fmt.Errorf("opening automatic Incident Investigations: %w", err)
	}
	return nil
}
