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

	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/changes"
)

// OpenInventoryScopes upserts one synchronization scope per Kubernetes Integration
// served by this registration and reports them, so the session can send one policy each.
func (p *Database) OpenInventoryScopes(
	ctx context.Context, organization tenancy.Organization,
	registrationID uuid.UUID, requestedInterval time.Duration,
) ([]changes.Scope, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return nil, err
	}
	seconds := int64(requestedInterval / time.Second)
	if seconds < 1 {
		return nil, fmt.Errorf("a non-positive synchronization interval cannot be requested")
	}

	rows, err := pool.Query(ctx, `
		WITH served AS (
			SELECT integration_id
			  FROM integration
			 WHERE org_id = $1
			   AND relay_id = $2
			   -- 2 is the kubernetes integration type, the one kind a Relay watches.
			   AND provider = 'kubernetes'
			   AND NOT disabled
		)
		INSERT INTO change_scope
			(integration_id, org_id, requested_interval_seconds, updated_at)
		SELECT integration_id, $1, $3, now() FROM served
		ON CONFLICT (integration_id) DO UPDATE
			SET requested_interval_seconds = EXCLUDED.requested_interval_seconds,
			    -- The revision moves exactly when the request changed, so a delta or a
			    -- freshness stamp can say which request it answered and "monotonic" stays
			    -- a true statement rather than a constant one.
			    policy_revision = change_scope.policy_revision
			        + CASE WHEN change_scope.requested_interval_seconds
			                    <> EXCLUDED.requested_interval_seconds THEN 1 ELSE 0 END,
			    updated_at = now()
		RETURNING integration_id, policy_revision, requested_interval_seconds`,
		organization.String(), registrationID, seconds)
	if err != nil {
		return nil, fmt.Errorf("opening inventory scopes: %w", err)
	}
	defer rows.Close()

	var scopes []changes.Scope
	for rows.Next() {
		var scope changes.Scope
		var intervalSeconds int64
		if err = rows.Scan(&scope.IntegrationID,
			&scope.PolicyRevision, &intervalSeconds); err != nil {
			return nil, fmt.Errorf("reading an inventory scope: %w", err)
		}
		scope.RequestedInterval = time.Duration(intervalSeconds) * time.Second
		scopes = append(scopes, scope)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("opening inventory scopes: %w", err)
	}
	return scopes, nil
}

// RecordInventoryDelta records one at-least-once delta, deduplicated by observation.
//
// The Integration check and the writes share one transaction, and the check is the
// same shape EnqueueJob uses in reverse: the delta is recorded only if the Integration
// it names belongs to this organization, is served by this registration and is not
// disabled. A delta failing that is REFUSED but still acknowledged —
// the Relay can do nothing about it, and resending forever helps nobody; the refusal is
// the log's to report.
//
// A redelivery collapses row by row against the dedup key, so recording is idempotent
// without any notion of a delta having been seen before.
func (p *Database) RecordInventoryDelta(
	ctx context.Context, organization tenancy.Organization,
	registrationID uuid.UUID, delta changes.Delta,
) (changes.Recorded, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return changes.Recorded{}, err
	}

	transaction, err := pool.Begin(ctx)
	if err != nil {
		return changes.Recorded{}, fmt.Errorf("beginning a change delta: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	var served bool
	err = transaction.QueryRow(ctx, `
		SELECT TRUE
		  FROM integration
		 WHERE integration_id = $1
		   AND org_id = $2
		   AND relay_id = $3
		   AND NOT disabled`,
		delta.IntegrationID, organization.String(), registrationID).Scan(&served)
	if err == pgx.ErrNoRows {
		return changes.Recorded{Refused: true}, nil
	}
	if err != nil {
		return changes.Recorded{}, fmt.Errorf("resolving a delta's integration: %w", err)
	}

	// A fixed write order, for the same reason alertEvents are sorted: two chunks carrying
	// overlapping objects must not take row locks in opposite orders.
	ordered := slices.SortedFunc(slices.Values(delta.Changes), compareChanges)
	inserted := 0
	for _, change := range ordered {
		kind := int16(change.Change)
		if delta.Baseline {
			kind = int16(changes.ChangeBaseline)
		}
		fields, marshalErr := json.Marshal(orEmptyFields(change.Fields))
		if marshalErr != nil {
			return changes.Recorded{}, fmt.Errorf("encoding field changes: %w", marshalErr)
		}
		tag, execErr := transaction.Exec(ctx, `
			INSERT INTO change_event
				(org_id, integration_id, namespace, object_kind,
				 object_name, object_uid, observed_revision, change_kind, observed_at, fields)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			ON CONFLICT (integration_id, object_uid, observed_revision) DO NOTHING`,
			organization.String(), delta.IntegrationID, change.Namespace,
			int16(change.Kind), change.Name, change.UID, change.ObservedRevision, kind,
			delta.ObservedAt, fields)
		if execErr != nil {
			return changes.Recorded{}, fmt.Errorf("recording a change event: %w", execErr)
		}
		inserted += int(tag.RowsAffected())
	}

	if err = advanceChangeScope(
		ctx, transaction, organization, delta, inserted,
	); err != nil {
		return changes.Recorded{}, err
	}

	if err = transaction.Commit(ctx); err != nil {
		return changes.Recorded{}, fmt.Errorf("committing a change delta: %w", err)
	}
	return changes.Recorded{Inserted: inserted}, nil
}

func advanceChangeScope(
	ctx context.Context, transaction pgx.Tx, organization tenancy.Organization,
	delta changes.Delta, inserted int,
) error {
	var err error
	if delta.Baseline {
		// Continuity is decided by what the baseline changed AND by how long nobody was
		// watching. A re-baseline that collapsed entirely proved no watched field moved —
		// declared-intent revisions only advance — but a collapse cannot prove an object was
		// not DELETED in the gap, so the boundary survives only a gap short enough (two
		// requested intervals past the last confirmation) that the unprovable window is
		// bounded by the scope's own cadence. Any insert, or a longer silence, moves the
		// boundary to where watching demonstrably resumed.
		_, err = transaction.Exec(ctx, `
			UPDATE change_scope
			   SET covered_since = CASE
			           WHEN covered_since IS NULL
			             OR $3 > 0
			             OR last_confirmed_at IS NULL
			             OR $2::timestamptz - last_confirmed_at >
			                    make_interval(secs => requested_interval_seconds * 2)
			           THEN $2::timestamptz
			           ELSE covered_since
			       END,
			       baseline_at = GREATEST(coalesce(baseline_at, $2), $2),
			       last_confirmed_at = GREATEST(coalesce(last_confirmed_at, $2), $2),
			       policy_revision = GREATEST(policy_revision, $4),
			       updated_at = now()
			 WHERE integration_id = $1 AND org_id = $5`,
			delta.IntegrationID, delta.ObservedAt, inserted, delta.PolicyRevision,
			organization.String())
	} else {
		_, err = transaction.Exec(ctx, `
			UPDATE change_scope
			   SET last_confirmed_at = GREATEST(coalesce(last_confirmed_at, $2), $2),
			       updated_at = now()
			 WHERE integration_id = $1 AND org_id = $3`,
			delta.IntegrationID, delta.ObservedAt, organization.String())
	}
	if err != nil {
		return fmt.Errorf("advancing the scope's coverage: %w", err)
	}
	return nil
}

// RecordInventoryFreshness applies a heartbeat's per-scope stamps. The guard subquery
// is the tenancy and serving check: a stamp naming an Integration this registration
// does not serve updates nothing.
func (p *Database) RecordInventoryFreshness(
	ctx context.Context, organization tenancy.Organization,
	registrationID uuid.UUID, stamps []changes.Freshness,
) error {
	if len(stamps) == 0 {
		return nil
	}
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}
	for _, stamp := range stamps {
		var confirmed *time.Time
		if stamp.CompletedAt != nil && !stamp.Faulted {
			confirmed = stamp.CompletedAt
		}
		if _, err = pool.Exec(ctx, `
			UPDATE change_scope
			   SET last_confirmed_at = GREATEST(coalesce(last_confirmed_at, $2), coalesce($2, last_confirmed_at)),
			       faulted = $3,
			       truncated = $4,
			       updated_at = now()
			 WHERE integration_id = $1
			   AND org_id = $5
			   AND integration_id IN (
			       SELECT integration_id FROM integration
			        WHERE org_id = $5 AND relay_id = $6)`,
			stamp.IntegrationID, confirmed, stamp.Faulted, stamp.Truncated,
			organization.String(), registrationID); err != nil {
			return fmt.Errorf("recording inventory freshness: %w", err)
		}
	}
	return nil
}

// RecentChanges answers the question: what changed in this namespace, through
// this Integration, in this window. Baselines are excluded — they record where
// watching began, not something changing — and the scope's boundaries travel with the
// answer so an empty list is readable as "nothing changed" only where that is actually
// knowable.
func (p *Database) RecentChanges(
	ctx context.Context, organization tenancy.Organization,
	integrationID uuid.UUID, namespace string, from, to time.Time, limit int,
) (changes.WindowChanges, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return changes.WindowChanges{}, err
	}
	if limit < 1 {
		return changes.WindowChanges{}, fmt.Errorf("a change window needs a positive bound")
	}

	answer := changes.WindowChanges{}
	err = pool.QueryRow(ctx, `
		SELECT integration_id, policy_revision, requested_interval_seconds,
		       covered_since, baseline_at, last_confirmed_at, faulted, truncated
		  FROM change_scope
		 WHERE integration_id = $1 AND org_id = $2`,
		integrationID, organization.String()).Scan(
		&answer.Scope.IntegrationID, &answer.Scope.PolicyRevision,
		&scanSeconds{&answer.Scope.RequestedInterval}, &answer.Scope.CoveredSince,
		&answer.Scope.BaselineAt, &answer.Scope.LastConfirmedAt,
		&answer.Scope.Faulted, &answer.Scope.Truncated)
	if err == pgx.ErrNoRows {
		return changes.WindowChanges{}, nil
	}
	if err != nil {
		return changes.WindowChanges{}, fmt.Errorf("reading the change scope: %w", err)
	}
	answer.Covered = answer.Scope.CoveredSince != nil

	rows, err := pool.Query(ctx, `
		SELECT change_event_id, integration_id, namespace, object_kind, object_name,
		       object_uid, observed_revision, change_kind, observed_at, received_at, fields
		  FROM change_event
		 WHERE integration_id = $1
		   AND org_id = $2
		   AND namespace = $3
		   AND change_kind <> 1
		   AND observed_at >= $4
		   AND observed_at < $5
		 ORDER BY observed_at, change_event_id
		 LIMIT $6`,
		integrationID, organization.String(), namespace, from, to, limit+1)
	if err != nil {
		return changes.WindowChanges{}, fmt.Errorf("reading the change window: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var event changes.Event
		var kind, change int16
		var fields []byte
		if err = rows.Scan(&event.ID, &event.IntegrationID,
			&event.Namespace, &kind, &event.Name, &event.UID, &event.ObservedRevision,
			&change, &event.ObservedAt, &event.RecordedAt, &fields); err != nil {
			return changes.WindowChanges{}, fmt.Errorf("reading a change event: %w", err)
		}
		event.Kind = changes.ObjectKind(kind)
		event.Change = changes.ChangeKind(change)
		if err = json.Unmarshal(fields, &event.Fields); err != nil {
			return changes.WindowChanges{}, fmt.Errorf("decoding field changes: %w", err)
		}
		answer.Events = append(answer.Events, event)
	}
	if err = rows.Err(); err != nil {
		return changes.WindowChanges{}, fmt.Errorf("reading the change window: %w", err)
	}
	if len(answer.Events) > limit {
		answer.Events = answer.Events[:limit]
		answer.Truncated = true
	}
	return answer, nil
}

// PruneChangesBefore removes at most limit events older than the horizon, oldest
// first. Purely by age: captured changes are derived operational
// context on its own retention schedule, and a pruned event is recoverable as a fresh
// baseline the next time a Relay observes the object.
func (p *Database) PruneChangesBefore(
	ctx context.Context, before time.Time, limit int,
) (int64, error) {
	if limit <= 0 {
		return 0, nil
	}
	tag, err := p.pool.Exec(ctx, `
			DELETE FROM change_event
			 WHERE change_event_id IN (
			       SELECT change_event_id
			         FROM change_event
			        WHERE received_at < $1
			        ORDER BY received_at, change_event_id
			        LIMIT $2
			       )`, before, limit)
	if err != nil {
		return 0, fmt.Errorf("pruning the changes: %w", err)
	}
	return tag.RowsAffected(), nil
}

// compareChanges orders two changes by the identity they are written under — the dedup
// key's own order.
func compareChanges(a, b changes.Change) int {
	if byUID := strings.Compare(a.UID, b.UID); byUID != 0 {
		return byUID
	}
	return strings.Compare(a.ObservedRevision, b.ObservedRevision)
}

func orEmptyFields(fields []changes.FieldChange) []changes.FieldChange {
	if fields == nil {
		return []changes.FieldChange{}
	}
	return fields
}

// scanSeconds reads an integer seconds column into a duration, so the interval is one
// type inside the program and one honest unit in the schema.
type scanSeconds struct{ into *time.Duration }

func (s *scanSeconds) Scan(value any) error {
	switch v := value.(type) {
	case int64:
		*s.into = time.Duration(v) * time.Second
		return nil
	case int32:
		*s.into = time.Duration(v) * time.Second
		return nil
	default:
		return fmt.Errorf("requested_interval_seconds held %T", value)
	}
}

// WorkloadInventory reads a bounded digest of the current workload
// identities — each rendered with its Integration and "namespace/kind name" — for the autonomous
// orientation. A navigation index, never evidence: deletions drop out, and only the
// watched workload kinds appear. Empty when no Relay has synchronized anything.
func (p *Database) WorkloadInventory(
	ctx context.Context, organization tenancy.Organization, limit int,
) ([]string, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, `
		SELECT latest.integration_id, scope.covered_since, scope.last_confirmed_at,
		       scope.faulted, scope.truncated, namespace, object_kind, object_name
		  FROM (
		      SELECT DISTINCT ON (integration_id, namespace, object_kind, object_name)
		             integration_id, namespace, object_kind, object_name, change_kind
		        FROM change_event
		       WHERE org_id = $1 AND object_kind IN ($2, $3, $4)
		       ORDER BY integration_id, namespace, object_kind, object_name, observed_at DESC, change_event_id DESC
		  ) latest
		  JOIN change_scope scope
		    ON scope.org_id = $1 AND scope.integration_id = latest.integration_id
		 WHERE change_kind <> $5
		 ORDER BY namespace, object_name, latest.integration_id
		 LIMIT $6`,
		organization.String(), int16(changes.KindDeployment),
		int16(changes.KindStatefulSet), int16(changes.KindDaemonSet),
		int16(changes.ChangeDeleted), limit)
	if err != nil {
		return nil, fmt.Errorf("reading the workload inventory: %w", err)
	}
	defer rows.Close()

	var digest []string
	for rows.Next() {
		var integration uuid.UUID
		var namespace, name string
		var coveredSince, lastConfirmed *time.Time
		var faulted, truncated bool
		var kind int16
		if err := rows.Scan(&integration, &coveredSince, &lastConfirmed,
			&faulted, &truncated, &namespace, &kind, &name); err != nil {
			return nil, fmt.Errorf("reading a workload identity: %w", err)
		}
		digest = append(digest, "integration "+integration.String()+" "+
			inventoryCoverage(coveredSince, lastConfirmed, faulted, truncated)+" "+
			namespace+"/"+changes.ObjectKind(kind).String()+" "+name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the workload inventory: %w", err)
	}
	return digest, nil
}

func inventoryCoverage(
	coveredSince, lastConfirmed *time.Time, faulted, truncated bool,
) string {
	var parts []string
	switch {
	case faulted:
		parts = append(parts, "coverage faulted")
	case coveredSince != nil:
		parts = append(parts, "covered since "+coveredSince.UTC().Format(time.RFC3339))
	default:
		parts = append(parts, "coverage unknown")
	}
	if lastConfirmed != nil {
		parts = append(parts, "last confirmed "+lastConfirmed.UTC().Format(time.RFC3339))
	}
	if truncated {
		parts = append(parts, "truncated")
	}
	return strings.Join(parts, " ")
}
