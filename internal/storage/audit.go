package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// The two refusals this file produces, named here for the call sites in this package and owned
// by the capabilities that define them.
//
// ErrNotAMember is the tenancy boundary answering inside storage, one layer below the
// middleware that already asked the same question. The duplication is deliberate: the
// middleware covers every route from a table, and this covers every call — including one made
// from a path nobody routed through the middleware. Handlers map it to 404, never 403.
//
// They are the OWNING packages' values rather than copies, so a capability that recognises
// authz.ErrNotAMember without importing persistence — which ADR-017 requires of it — matches
// the same error this package returns.
var (
	ErrNotAMember  = authz.ErrNotAMember
	ErrAuditFailed = audit.ErrWriteFailed
)

// RecordEvent writes one event on its own. It is what the authorization middleware uses for a
// refusal — a denial has no operation to be inside the transaction of — and nothing else
// should reach for it: a state change records itself through audited, in the transaction that
// made it.
func (p *Placements) RecordEvent(
	ctx context.Context, organization tenancy.Organization, event audit.Event,
) error {
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}
	return writeEvent(ctx, pool, event)
}

// writeEvent inserts one row. It is deliberately not exported: the two ways an event reaches
// the database are RecordEvent and audited, and a third would be a way to record a change
// outside its transaction.
func writeEvent(ctx context.Context, on executor, event audit.Event) error {
	bounded := event.Bounded()

	detail, err := json.Marshal(orEmptyDetail(bounded.Detail))
	if err != nil {
		return fmt.Errorf("%w: encoding the detail: %w", ErrAuditFailed, err)
	}
	occurred := bounded.OccurredAt
	if occurred.IsZero() {
		occurred = time.Now().UTC()
	}

	if _, err := on.Exec(ctx, `
		INSERT INTO audit_event (event_id, organization, actor_kind, actor_id,
		                         actor_display_name, action, target_kind, target_id, outcome,
		                         source_address, request_id, detail, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		uuid.New(), bounded.Organization, int16(bounded.Actor.Kind), bounded.Actor.ID,
		bounded.Actor.DisplayName, string(bounded.Action), string(bounded.Target.Kind),
		bounded.Target.ID, int16(bounded.Outcome), bounded.SourceAddress, bounded.RequestID,
		detail, occurred); err != nil {
		return fmt.Errorf("%w: %w", ErrAuditFailed, err)
	}
	return nil
}

func orEmptyDetail(detail audit.Detail) audit.Detail {
	if detail == nil {
		return audit.Detail{}
	}
	return detail
}

// audited runs a mutation and the audit row recording it inside ONE transaction.
//
// Two properties follow and both are the point. An operation nobody could attribute never
// happens: if the event cannot be written, the change it describes rolls back. And an event
// describing an operation that failed is never written: the transaction carries both or
// neither.
//
// The membership check happens before the transaction opens, so a principal with no
// membership never reaches a statement — the boundary refuses before the work rather than
// after it.
//
// mutate returns the target the event names, because for a creation the identifier does not
// exist until the insert has run and an event written beforehand would have to guess it.
func audited[T any](
	ctx context.Context,
	p *Placements,
	principal authz.Principal,
	organization tenancy.Organization,
	action audit.Action,
	mutate func(context.Context, pgx.Tx) (T, audit.Target, audit.Detail, error),
) (T, error) {
	var zero T
	if !principal.MemberOf(organization) {
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

	result, target, detail, err := mutate(ctx, transaction)
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

// AuditEvents reads a tenant's record, newest first.
//
// It takes the principal for the same reason every operator-facing read does: the middleware
// has already decided, and this is the layer that cannot be reached around.
func (p *Placements) AuditEvents(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	page audit.Page,
) (audit.List, error) {
	if !principal.MemberOf(organization) {
		return audit.List{}, ErrNotAMember
	}
	pool, err := p.Pool(organization)
	if err != nil {
		return audit.List{}, err
	}
	before, beforeID, err := decodeCursor(page.After)
	if err != nil {
		return audit.List{}, err
	}

	limit := pageLimit(page.Limit)
	rows, err := pool.Query(ctx, `
		SELECT event_id, actor_kind, actor_id, actor_display_name, action, target_kind,
		       target_id, outcome, source_address, request_id, detail, occurred_at
		  FROM audit_event
		 WHERE organization = $1
		   AND ($2::TIMESTAMPTZ IS NULL
		        OR (occurred_at, event_id) < ($2::TIMESTAMPTZ, $3::UUID))
		 ORDER BY occurred_at DESC, event_id DESC
		 LIMIT $4`,
		organization.String(), before, beforeID, limit+1)
	if err != nil {
		return audit.List{}, fmt.Errorf("reading the audit trail: %w", err)
	}
	defer rows.Close()

	// One row past the page is read rather than counted, so "is there more" is answered by the
	// same query that answers "what is on this page".
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
			next = encodeCursor(lastAt, lastID)
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

// THE ONE PATH THROUGH WHICH AN AUDIT EVENT MAY LEAVE.
//
// Migration 0011 refuses an UPDATE, a DELETE and a TRUNCATE on audit_event outright, except in a
// transaction that has declared itself the retention pruner. These two functions are that path.
// Everything else in this program — every handler, every worker, every future refactor — is
// refused by the database rather than by a reviewer noticing.

// DeclaredRetentions reports every tenant that has declared how long it keeps the record.
//
// It names no organization, for the same reason InvestigationsAwaitingWork does not: its job is
// to discover which tenants there are, so there is no tenant in the question to resolve a
// placement from. It reads no tenant data — only which tenants declared a number.
//
// A tenant whose declared period is zero is NOT reported. Zero is the product's default, which is
// to keep everything, and treating it as a horizon of "now" would delete an entire record because
// somebody had never set a policy.
//
// A placement that cannot be read fails the whole scan rather than being skipped. Continuing would
// apply some tenants' schedules and silently not others, which is worse than applying none: an
// operator reading a successful sweep would believe the schedule was kept everywhere.
func (p *Placements) DeclaredRetentions(ctx context.Context) ([]audit.Retention, error) {
	seen := make(map[string]struct{})
	var declared []audit.Retention

	// A fixed order, so two deployments of the same configuration behave alike.
	for _, name := range p.names() {
		rows, err := p.pools[name].Query(ctx, `
			SELECT organization, audit_retention_days
			  FROM organization_policy
			 WHERE audit_retention_days > 0
			 ORDER BY organization`)
		if err != nil {
			return nil, fmt.Errorf("reading retention schedules in placement %q: %w", name, err)
		}
		for rows.Next() {
			var id string
			var days int
			if err = rows.Scan(&id, &days); err != nil {
				rows.Close()
				return nil, fmt.Errorf("reading a retention schedule: %w", err)
			}
			if _, already := seen[id]; already {
				continue
			}
			organization, parseErr := tenancy.NewOrganization(id)
			if parseErr != nil {
				// Unreachable while every write validates the organization first, and not a
				// fallback: a policy row whose tenant cannot be named is not one to delete by.
				continue
			}
			seen[id] = struct{}{}
			declared = append(declared, audit.Retention{Organization: organization, Days: days})
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return nil, fmt.Errorf("reading retention schedules in placement %q: %w", name, err)
		}
	}
	return declared, nil
}

// PruneEventsBefore removes at most limit of one tenant's events older than the horizon.
//
// The declaration is made with set_config's LOCAL flag, so it lives for this transaction and no
// longer. That is the load-bearing detail: a session-level setting would survive on a pooled
// connection and turn every later transaction on it into one permitted to delete the record,
// which would make the append-only guarantee depend on which connection a request happened to get.
//
// The delete is bounded by an inner select rather than by the horizon alone, so one statement
// cannot take a lock proportional to a backlog. The order is oldest first, so a backlog worked
// through over several sweeps always removes what aged out longest ago.
func (p *Placements) PruneEventsBefore(
	ctx context.Context, organization tenancy.Organization, before time.Time, limit int,
) (int64, error) {
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
		        WHERE organization = $1 AND occurred_at < $2
		        ORDER BY occurred_at, event_id
		        LIMIT $3
		       )`,
		organization.String(), before, limit)
	if err != nil {
		return 0, fmt.Errorf("pruning audit events: %w", err)
	}
	if err = transaction.Commit(ctx); err != nil {
		return 0, fmt.Errorf("committing an audit prune: %w", err)
	}
	return tag.RowsAffected(), nil
}
