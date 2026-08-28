package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
)

type WebhookDeliveryState string

const (
	WebhookDeliveryAccepted   WebhookDeliveryState = "accepted"
	WebhookDeliveryProcessing WebhookDeliveryState = "processing"
	WebhookDeliverySucceeded  WebhookDeliveryState = "succeeded"
	WebhookDeliveryFailed     WebhookDeliveryState = "failed"
)

var ErrWebhookDeliveryUnknown = errors.New("webhook delivery not found")

type WebhookDelivery struct {
	ID               uuid.UUID
	IntegrationID    uuid.UUID
	ProviderIdentity string
	LifecyclePhase   string
	RequestID        string
	State            WebhookDeliveryState
	Attempts         int
	FailureClass     string
	ReceivedAt       time.Time
	LastAttemptAt    *time.Time
	NextEligibleAt   *time.Time
}

type WebhookDeliveryPage struct {
	Deliveries []WebhookDelivery
	Next       string
}

func (d *Database) WebhookDeliveries(
	ctx context.Context, organization tenancy.Organization, state WebhookDeliveryState, page Page,
) (WebhookDeliveryPage, error) {
	after, afterID, err := decodeCursor(page.After)
	if err != nil {
		return WebhookDeliveryPage{}, ErrBadCursor
	}
	pool, err := d.Pool(organization)
	if err != nil {
		return WebhookDeliveryPage{}, err
	}
	limit := pageLimit(page.Limit)
	rows, err := pool.Query(ctx, deliveryProjection+`
		SELECT delivery_id, integration_id, provider_identity, lifecycle_phase, request_id,
		       state, attempts, failure_class, received_at,
		       last_attempt_at, next_eligible_at
		  FROM projected
		 WHERE ($2 = '' OR state = $2)
		   AND ($3::timestamptz IS NULL OR (received_at, delivery_id) < ($3, $4))
		 ORDER BY received_at DESC, delivery_id DESC LIMIT $5`,
		organization.String(), string(state), after, afterID, limit+1)
	if err != nil {
		return WebhookDeliveryPage{}, fmt.Errorf("listing webhook deliveries: %w", err)
	}
	defer rows.Close()
	found := make([]WebhookDelivery, 0, limit+1)
	for rows.Next() {
		var delivery WebhookDelivery
		if err := scanWebhookDelivery(rows, &delivery); err != nil {
			return WebhookDeliveryPage{}, fmt.Errorf("scanning webhook delivery: %w", err)
		}
		found = append(found, delivery)
	}
	if err := rows.Err(); err != nil {
		return WebhookDeliveryPage{}, err
	}
	result := WebhookDeliveryPage{Deliveries: found}
	if len(found) > limit {
		last := found[limit-1]
		result.Deliveries = found[:limit]
		result.Next = encodeCursor(last.ReceivedAt, last.ID)
	}
	return result, nil
}

func (d *Database) WebhookDeliveryByID(
	ctx context.Context, organization tenancy.Organization, deliveryID uuid.UUID,
) (WebhookDelivery, error) {
	pool, err := d.Pool(organization)
	if err != nil {
		return WebhookDelivery{}, err
	}
	var delivery WebhookDelivery
	err = scanWebhookDelivery(pool.QueryRow(ctx, deliveryProjection+`
		SELECT delivery_id, integration_id, provider_identity, lifecycle_phase, request_id,
		       state, attempts, failure_class, received_at,
		       last_attempt_at, next_eligible_at
		  FROM projected WHERE delivery_id = $2`, organization.String(), deliveryID), &delivery)
	if errors.Is(err, pgx.ErrNoRows) {
		return WebhookDelivery{}, ErrWebhookDeliveryUnknown
	}
	if err != nil {
		return WebhookDelivery{}, fmt.Errorf("reading webhook delivery: %w", err)
	}
	return delivery, nil
}

const deliveryProjection = `
	WITH projected AS (
		SELECT delivery.delivery_id, delivery.integration_id,
		       delivery.provider_identity, delivery.lifecycle_phase, delivery.request_id,
		       delivery.received_at,
		       CASE
		         WHEN bool_or(work.status = 4) THEN 'failed'
		         WHEN bool_or(work.status IN (2, 3)) OR
		              (bool_or(work.status = 5) AND bool_or(work.status = 1)) THEN 'processing'
		         WHEN count(work.work_id) = 0 OR bool_and(work.status = 5) THEN 'succeeded'
		         WHEN bool_and(work.status = 1) THEN 'accepted'
		         ELSE 'processing'
		       END AS state,
		       coalesce(max(work.attempts), 0) AS attempts,
		       CASE WHEN count(DISTINCT work.failure_class) FILTER (WHERE work.status = 4) > 1
		            THEN 'multiple'
		            ELSE coalesce(min(work.failure_class) FILTER (WHERE work.status = 4), '')
		       END AS failure_class,
		       max(work.updated_at) FILTER (WHERE work.attempts > 0) AS last_attempt_at,
		       min(work.available_at) FILTER (WHERE work.status = 3) AS next_eligible_at
		  FROM integration_delivery AS delivery
		  LEFT JOIN webhook_work AS work
		    ON work.org_id = delivery.org_id AND work.delivery_id = delivery.delivery_id
		 WHERE delivery.org_id = $1 AND delivery.outcome = 1
		 GROUP BY delivery.delivery_id, delivery.integration_id, delivery.provider_identity,
		          delivery.lifecycle_phase, delivery.request_id, delivery.received_at
	)`

type rowScanner interface{ Scan(...any) error }

func scanWebhookDelivery(row rowScanner, delivery *WebhookDelivery) error {
	return row.Scan(&delivery.ID, &delivery.IntegrationID, &delivery.ProviderIdentity,
		&delivery.LifecyclePhase, &delivery.RequestID, &delivery.State, &delivery.Attempts,
		&delivery.FailureClass, &delivery.ReceivedAt,
		&delivery.LastAttemptAt, &delivery.NextEligibleAt)
}

func (d *Database) ReplayWebhookDelivery(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	deliveryID uuid.UUID,
) error {
	_, err := audited(ctx, d, principal, organization, audit.ActionWebhookDeliveryReplayed,
		func(ctx context.Context, tx pgx.Tx) (struct{}, audit.Target, audit.Detail, error) {
			rows, updateErr := tx.Query(ctx, `
				UPDATE webhook_work
				   SET status = 1, attempts = 0, available_at = now(),
				       failure_class = '', failure_message = '', updated_at = now()
				 WHERE org_id = $1 AND delivery_id = $2 AND status = 4
				 RETURNING integration_id, failure_class, attempts`,
				organization.String(), deliveryID)
			if updateErr != nil {
				return struct{}{}, audit.Target{}, nil, updateErr
			}
			defer rows.Close()
			var integrationID uuid.UUID
			var replayed int
			for rows.Next() {
				var ignoredClass string
				var ignoredAttempts int
				if scanErr := rows.Scan(&integrationID, &ignoredClass, &ignoredAttempts); scanErr != nil {
					return struct{}{}, audit.Target{}, nil, scanErr
				}
				replayed++
			}
			if rowsErr := rows.Err(); rowsErr != nil {
				return struct{}{}, audit.Target{}, nil, rowsErr
			}
			if replayed == 0 {
				return struct{}{}, audit.Target{}, nil, pgx.ErrNoRows
			}
			return struct{}{}, audit.Target{Kind: audit.TargetWebhookDelivery, ID: deliveryID.String()},
				audit.Detail{"integrationId": integrationID.String(), "workReplayed": replayed}, nil
		})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrWebhookDeliveryUnknown
	}
	return err
}
