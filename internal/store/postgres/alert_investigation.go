package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// AlertAdmissionPolicy bounds automatic Investigations opened by one delivery.
type AlertAdmissionPolicy struct {
	WindowLead     time.Duration
	MaximumPending int
}

// AlertCapacityError means the current backlog cannot fit the delivery's Investigations.
type AlertCapacityError struct{}

func (AlertCapacityError) Error() string { return "pending Investigation capacity exhausted" }

// AlertBatchTooLargeError means the delivery cannot fit even an empty pending queue.
type AlertBatchTooLargeError struct{}

func (AlertBatchTooLargeError) Error() string {
	return "alert batch exceeds pending Investigation limit"
}

func openAlertInvestigations(
	ctx context.Context, tx pgx.Tx, organization uuid.UUID,
	incidents []uuid.UUID, policy AlertAdmissionPolicy,
) error {
	if len(incidents) == 0 {
		return nil
	}
	if policy.MaximumPending > 0 && len(incidents) > policy.MaximumPending {
		return AlertBatchTooLargeError{}
	}
	if err := reserveWaitingInvestigations(ctx, tx, organization, policy.MaximumPending, len(incidents)); err != nil {
		if errors.Is(err, ErrWebhookJobCapacity) {
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
