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

	"github.com/open-cluster/oc-control-plane/internal/changeledger"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// The change ledger's persistence. The vocabulary lives in internal/changeledger; this
// file reconstructs it from rows and writes it into them, and decides nothing about
// what a change means.

// OpenInventoryScopes upserts one synchronization scope per Kubernetes Integration
// served by this registration and reports them, so the session can send one policy each.
func (p *Placements) OpenInventoryScopes(
	ctx context.Context, organization tenancy.Organization,
	registrationID uuid.UUID, requestedInterval time.Duration,
) ([]changeledger.Scope, error) {
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
			   AND integration_type_id = 2
			   AND disabled_at IS NULL
		)
		INSERT INTO change_ledger_scope
			(integration_id, org_id, requested_interval_seconds)
		SELECT integration_id, $1, $3 FROM served
		ON CONFLICT (integration_id) DO UPDATE
			SET requested_interval_seconds = EXCLUDED.requested_interval_seconds,
			    -- The revision moves exactly when the request changed, so a delta or a
			    -- freshness stamp can say which request it answered and "monotonic" stays
			    -- a true statement rather than a constant one.
			    policy_revision = change_ledger_scope.policy_revision
			        + CASE WHEN change_ledger_scope.requested_interval_seconds
			                    <> EXCLUDED.requested_interval_seconds THEN 1 ELSE 0 END,
			    updated_at = now()
		RETURNING integration_id, policy_revision, requested_interval_seconds`,
		organization.String(), registrationID, seconds)
	if err != nil {
		return nil, fmt.Errorf("opening inventory scopes: %w", err)
	}
	defer rows.Close()

	var scopes []changeledger.Scope
	for rows.Next() {
		var scope changeledger.Scope
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
func (p *Placements) RecordInventoryDelta(
	ctx context.Context, organization tenancy.Organization,
	registrationID uuid.UUID, delta changeledger.Delta,
) (changeledger.Recorded, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return changeledger.Recorded{}, err
	}

	transaction, err := pool.Begin(ctx)
	if err != nil {
		return changeledger.Recorded{}, fmt.Errorf("beginning a ledger delta: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	var served bool
	err = transaction.QueryRow(ctx, `
		SELECT TRUE
		  FROM integration
		 WHERE integration_id = $1
		   AND org_id = $2
		   AND relay_id = $3
		   AND disabled_at IS NULL`,
		delta.IntegrationID, organization.String(), registrationID).Scan(&served)
	if err == pgx.ErrNoRows {
		return changeledger.Recorded{Refused: true}, nil
	}
	if err != nil {
		return changeledger.Recorded{}, fmt.Errorf("resolving a delta's integration: %w", err)
	}

	// A fixed write order, for the same reason signals are sorted: two chunks carrying
	// overlapping objects must not take row locks in opposite orders.
	ordered := slices.SortedFunc(slices.Values(delta.Changes), compareChanges)
	inserted := 0
	for _, change := range ordered {
		kind := int16(change.Change)
		if delta.Baseline {
			kind = int16(changeledger.ChangeBaseline)
		}
		fields, marshalErr := json.Marshal(orEmptyFields(change.Fields))
		if marshalErr != nil {
			return changeledger.Recorded{}, fmt.Errorf("encoding field changes: %w", marshalErr)
		}
		tag, execErr := transaction.Exec(ctx, `
			INSERT INTO change_ledger
				(org_id, integration_id, namespace, object_kind,
				 object_name, object_uid, observed_revision, change_kind, observed_at, fields)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			ON CONFLICT (integration_id, object_uid, observed_revision) DO NOTHING`,
			organization.String(), delta.IntegrationID, change.Namespace,
			int16(change.Kind), change.Name, change.UID, change.ObservedRevision, kind,
			delta.ObservedAt, fields)
		if execErr != nil {
			return changeledger.Recorded{}, fmt.Errorf("recording a ledger entry: %w", execErr)
		}
		inserted += int(tag.RowsAffected())
	}

	if delta.Baseline {
		// Continuity is decided by what the baseline changed AND by how long nobody was
		// watching. A re-baseline that collapsed entirely proved no watched field moved —
		// declared-intent revisions only advance — but a collapse cannot prove an object was
		// not DELETED in the gap, so the boundary survives only a gap short enough (two
		// requested intervals past the last confirmation) that the unprovable window is
		// bounded by the scope's own cadence. Any insert, or a longer silence, moves the
		// boundary to where watching demonstrably resumed.
		_, err = transaction.Exec(ctx, `
			UPDATE change_ledger_scope
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
			 WHERE integration_id = $1`,
			delta.IntegrationID, delta.ObservedAt, inserted, delta.PolicyRevision)
	} else {
		_, err = transaction.Exec(ctx, `
			UPDATE change_ledger_scope
			   SET last_confirmed_at = GREATEST(coalesce(last_confirmed_at, $2), $2),
			       updated_at = now()
			 WHERE integration_id = $1`,
			delta.IntegrationID, delta.ObservedAt)
	}
	if err != nil {
		return changeledger.Recorded{}, fmt.Errorf("advancing the scope's coverage: %w", err)
	}

	if err = transaction.Commit(ctx); err != nil {
		return changeledger.Recorded{}, fmt.Errorf("committing a ledger delta: %w", err)
	}
	return changeledger.Recorded{Inserted: inserted}, nil
}

// RecordInventoryFreshness applies a heartbeat's per-scope stamps. The guard subquery
// is the tenancy and serving check: a stamp naming an Integration this registration
// does not serve updates nothing.
func (p *Placements) RecordInventoryFreshness(
	ctx context.Context, organization tenancy.Organization,
	registrationID uuid.UUID, stamps []changeledger.Freshness,
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
			UPDATE change_ledger_scope
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

// RecentLedgerChanges answers the question: what changed in this namespace, through
// this Integration, in this window. Baselines are excluded — they record where
// watching began, not something changing — and the scope's boundaries travel with the
// answer so an empty list is readable as "nothing changed" only where that is actually
// knowable.
func (p *Placements) RecentLedgerChanges(
	ctx context.Context, organization tenancy.Organization,
	integrationID uuid.UUID, namespace string, from, to time.Time, limit int,
) (changeledger.WindowChanges, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return changeledger.WindowChanges{}, err
	}
	if limit < 1 {
		return changeledger.WindowChanges{}, fmt.Errorf("a ledger window needs a positive bound")
	}

	answer := changeledger.WindowChanges{}
	err = pool.QueryRow(ctx, `
		SELECT integration_id, policy_revision, requested_interval_seconds,
		       covered_since, baseline_at, last_confirmed_at, faulted, truncated
		  FROM change_ledger_scope
		 WHERE integration_id = $1 AND org_id = $2`,
		integrationID, organization.String()).Scan(
		&answer.Scope.IntegrationID, &answer.Scope.PolicyRevision,
		&scanSeconds{&answer.Scope.RequestedInterval}, &answer.Scope.CoveredSince,
		&answer.Scope.BaselineAt, &answer.Scope.LastConfirmedAt,
		&answer.Scope.Faulted, &answer.Scope.Truncated)
	if err == pgx.ErrNoRows {
		return changeledger.WindowChanges{}, nil
	}
	if err != nil {
		return changeledger.WindowChanges{}, fmt.Errorf("reading the ledger scope: %w", err)
	}
	answer.Covered = answer.Scope.CoveredSince != nil

	rows, err := pool.Query(ctx, `
		SELECT entry_id, integration_id, namespace, object_kind, object_name,
		       object_uid, observed_revision, change_kind, observed_at, received_at, fields
		  FROM change_ledger
		 WHERE integration_id = $1
		   AND org_id = $2
		   AND namespace = $3
		   AND change_kind <> 1
		   AND observed_at >= $4
		   AND observed_at < $5
		 ORDER BY observed_at, entry_id
		 LIMIT $6`,
		integrationID, organization.String(), namespace, from, to, limit+1)
	if err != nil {
		return changeledger.WindowChanges{}, fmt.Errorf("reading the ledger window: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var entry changeledger.Entry
		var kind, change int16
		var fields []byte
		if err = rows.Scan(&entry.ID, &entry.IntegrationID,
			&entry.Namespace, &kind, &entry.Name, &entry.UID, &entry.ObservedRevision,
			&change, &entry.ObservedAt, &entry.RecordedAt, &fields); err != nil {
			return changeledger.WindowChanges{}, fmt.Errorf("reading a ledger entry: %w", err)
		}
		entry.Kind = changeledger.ObjectKind(kind)
		entry.Change = changeledger.ChangeKind(change)
		if err = json.Unmarshal(fields, &entry.Fields); err != nil {
			return changeledger.WindowChanges{}, fmt.Errorf("decoding field changes: %w", err)
		}
		answer.Entries = append(answer.Entries, entry)
	}
	if err = rows.Err(); err != nil {
		return changeledger.WindowChanges{}, fmt.Errorf("reading the ledger window: %w", err)
	}
	if len(answer.Entries) > limit {
		answer.Entries = answer.Entries[:limit]
		answer.Truncated = true
	}
	return answer, nil
}

// PruneChangeLedgerBefore removes at most limit entries older than the horizon, across
// every placement, oldest first. Purely by age: the ledger is derived operational
// context on its own retention schedule, and a pruned entry is recoverable as a fresh
// baseline the next time a Relay observes the object.
func (p *Placements) PruneChangeLedgerBefore(
	ctx context.Context, before time.Time, limit int,
) (int64, error) {
	if limit <= 0 {
		return 0, nil
	}
	var removed int64
	for _, name := range p.names() {
		tag, err := p.pools[name].Exec(ctx, `
			DELETE FROM change_ledger
			 WHERE entry_id IN (
			       SELECT entry_id
			         FROM change_ledger
			        WHERE received_at < $1
			        ORDER BY received_at, entry_id
			        LIMIT $2
			       )`, before, limit)
		if err != nil {
			return removed, fmt.Errorf("pruning the change ledger in placement %q: %w", name, err)
		}
		removed += tag.RowsAffected()
	}
	return removed, nil
}

// compareChanges orders two changes by the identity they are written under — the dedup
// key's own order.
func compareChanges(a, b changeledger.Change) int {
	if byUID := strings.Compare(a.UID, b.UID); byUID != 0 {
		return byUID
	}
	return strings.Compare(a.ObservedRevision, b.ObservedRevision)
}

func orEmptyFields(fields []changeledger.FieldChange) []changeledger.FieldChange {
	if fields == nil {
		return []changeledger.FieldChange{}
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
