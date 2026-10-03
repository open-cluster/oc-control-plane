package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
)

var (
	ErrNotAMember  = authz.ErrNotAMember
	ErrAuditFailed = audit.ErrWriteFailed
)

func (p *Database) RecordEvent(
	ctx context.Context, organization uuid.UUID, event audit.Event,
) error {
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}
	return writeEvent(ctx, pool, event)
}

func writeEvent(ctx context.Context, on executor, event audit.Event) error {
	bounded := event.Bounded()
	bounded.Detail = orEmptyDetail(bounded.Detail)

	detail, err := json.Marshal(bounded.Detail)
	if err != nil {
		return fmt.Errorf("%w: encoding the detail: %w", ErrAuditFailed, err)
	}
	occurred := bounded.OccurredAt
	if occurred.IsZero() {
		occurred = time.Now().UTC()
	}
	bounded.OccurredAt = occurred

	eventID := uuid.New()
	if _, err := on.Exec(ctx, `
		INSERT INTO audit_event (event_id, org_id, actor_kind, actor_id,
		                         actor_display_name, action, target_kind, target_id, outcome,
		                         source_address, request_id, detail, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		eventID, nullableOrganization(bounded.Organization), int16(bounded.Actor.Kind), bounded.Actor.ID,
		bounded.Actor.DisplayName, string(bounded.Action), string(bounded.Target.Kind),
		bounded.Target.ID, int16(bounded.Outcome), bounded.SourceAddress, bounded.RequestID,
		detail, occurred); err != nil {
		return fmt.Errorf("%w: %w", ErrAuditFailed, err)
	}
	return nil
}

func nullableOrganization(organization string) any {
	if organization == "" {
		return nil
	}
	return organization
}

func orEmptyDetail(detail audit.Detail) audit.Detail {
	if detail == nil {
		return audit.Detail{}
	}
	return detail
}

func audited[T any](
	ctx context.Context,
	p *Database,
	principal authz.Principal,
	organization uuid.UUID,
	action audit.Action,
	mutate func(context.Context, pgx.Tx) (T, audit.Target, audit.Detail, error),
) (T, error) {
	// The mutation and audit event commit together, so neither can exist without the other.
	return auditedWithAction(ctx, p, principal, organization,
		func(ctx context.Context, transaction pgx.Tx) (
			T, audit.Action, audit.Target, audit.Detail, error,
		) {
			result, target, detail, err := mutate(ctx, transaction)
			return result, action, target, detail, err
		})
}

func auditedWithAction[T any](
	ctx context.Context,
	p *Database,
	principal authz.Principal,
	organization uuid.UUID,
	mutate func(context.Context, pgx.Tx) (T, audit.Action, audit.Target, audit.Detail, error),
) (T, error) {
	var zero T
	if principal.Organization() != organization {
		return zero, ErrNotAMember
	}
	pool, err := p.Pool(organization)
	if err != nil {
		return zero, err
	}

	transaction, err := pool.Begin(ctx)
	if err != nil {
		return zero, fmt.Errorf("begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = transaction.Rollback(ctx)
		}
	}()

	result, action, target, detail, err := mutate(ctx, transaction)
	if err != nil {
		return zero, err
	}
	if err := writeEvent(ctx, transaction, audit.Event{
		Organization:  organization.String(),
		Actor:         principal.Actor(),
		Action:        action,
		Target:        target,
		Outcome:       audit.OutcomeAllowed,
		SourceAddress: principal.SourceAddress(),
		RequestID:     principal.RequestID(),
		Detail:        detail,
	}); err != nil {
		return zero, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return zero, fmt.Errorf("commit: %w", err)
	}
	committed = true
	return result, nil
}

func (p *Database) AuditEvents(
	ctx context.Context, principal authz.Principal, organization uuid.UUID,
	page audit.Page,
) (audit.List, error) {
	if principal.Organization() != organization {
		return audit.List{}, ErrNotAMember
	}
	pool, err := p.Pool(organization)
	if err != nil {
		return audit.List{}, err
	}
	before, beforeID, err := decodeCursor(page.After, "-occurredAt")
	if err != nil {
		return audit.List{}, err
	}

	limit := pageLimit(page.Limit)
	rows, err := pool.Query(ctx, `
		SELECT event_id, actor_kind, actor_id, actor_display_name, action, target_kind,
		       target_id, outcome, source_address, request_id, detail, occurred_at
		  FROM audit_event
		 WHERE org_id = $1
		   AND ($2::TIMESTAMPTZ IS NULL
		        OR (occurred_at, event_id) < ($2::TIMESTAMPTZ, $3::UUID))
		 ORDER BY occurred_at DESC, event_id DESC
		 LIMIT $4`,
		organization, before, beforeID, limit+1)
	if err != nil {
		return audit.List{}, fmt.Errorf("reading the audit trail: %w", err)
	}
	defer rows.Close()

	events := make([]audit.Recorded, 0, limit)
	var (
		next   string
		lastAt time.Time
		lastID uuid.UUID
	)
	for rows.Next() {
		var (
			id       uuid.UUID
			recorded audit.Recorded
			kind     int16
			outcome  int16
			action   string
			target   string
			detail   []byte
		)
		if err := rows.Scan(&id, &kind, &recorded.Actor.ID, &recorded.Actor.DisplayName,
			&action, &target, &recorded.Target.ID, &outcome, &recorded.SourceAddress,
			&recorded.RequestID, &detail, &recorded.OccurredAt); err != nil {
			return audit.List{}, fmt.Errorf("scanning an audit event: %w", err)
		}
		if len(events) == limit {
			next = encodeCursor("-occurredAt", lastAt, lastID)
			break
		}
		recorded.ID = id.String()
		recorded.Organization = organization.String()
		recorded.Actor.Kind = audit.ActorKind(kind)
		recorded.Action = audit.Action(action)
		recorded.Target.Kind = audit.TargetKind(target)
		recorded.Outcome = audit.Outcome(outcome)
		if err := json.Unmarshal(detail, &recorded.Detail); err != nil {
			return audit.List{}, fmt.Errorf("decoding an audit detail: %w", err)
		}
		events = append(events, recorded)
		lastAt, lastID = recorded.OccurredAt, id
	}
	if err := rows.Err(); err != nil {
		return audit.List{}, fmt.Errorf("reading the audit trail: %w", err)
	}
	return audit.List{Events: events, Next: next}, nil
}

func (p *Database) DeclaredRetentions(ctx context.Context) ([]audit.Retention, error) {
	var declared []audit.Retention
	rows, err := p.pool.Query(ctx, `
			SELECT org_id, audit_retention_days
			  FROM organization
			 WHERE audit_retention_days > 0
			 ORDER BY org_id`)
	if err != nil {
		return nil, fmt.Errorf("reading retention schedules: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var organization uuid.UUID
		var days int
		if err = rows.Scan(&organization, &days); err != nil {
			return nil, fmt.Errorf("reading a retention schedule: %w", err)
		}
		declared = append(declared, audit.Retention{Organization: organization, Days: days})
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("reading retention schedules: %w", err)
	}
	return declared, nil
}

func (p *Database) PruneEventsBefore(
	ctx context.Context, organization uuid.UUID, before time.Time, limit int,
) (int64, error) {
	// The pruning permission is transaction-local; a session setting could leak through the
	// connection pool and authorize unrelated deletes.
	pool, err := p.Pool(organization)
	if err != nil {
		return 0, err
	}
	if limit <= 0 {
		return 0, nil
	}

	transaction, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("beginning an audit prune: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	if _, err = transaction.Exec(ctx,
		`SELECT set_config('opencluster.audit_retention', 'pruning', TRUE)`); err != nil {
		return 0, fmt.Errorf("declaring the audit prune: %w", err)
	}

	tag, err := transaction.Exec(ctx, `
		DELETE FROM audit_event
		 WHERE event_id IN (
		       SELECT event_id
		         FROM audit_event
		        WHERE org_id = $1 AND occurred_at < $2
		        ORDER BY occurred_at, event_id
		        LIMIT $3
		       )`,
		organization, before, limit)
	if err != nil {
		return 0, fmt.Errorf("pruning audit events: %w", err)
	}
	if err = transaction.Commit(ctx); err != nil {
		return 0, fmt.Errorf("committing an audit prune: %w", err)
	}
	return tag.RowsAffected(), nil
}
