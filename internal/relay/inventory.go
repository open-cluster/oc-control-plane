package relay

import (
	"log/slog"
	"time"

	"github.com/google/uuid"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"

	"github.com/open-cluster/oc-control-plane/internal/changeledger"
)

// The change ledger's session half: policies out at greeting, deltas in as the third
// durable write this stream carries, freshness off the heartbeat. Nothing here is
// leased or fenced — a delta is at-least-once with a dedup key, so recording is
// idempotent and an ack is safe the moment the transaction commits.

// refreshInventoryPolicies keeps a live session's policies current: once at greeting, then
// on the synchronization cadence. The repeat is not a retry — it is what lets a Connection
// created or re-bound WHILE its relay is connected start being watched now, rather than at
// the relay's next reconnect, which a healthy relay may not make for weeks. Resending is
// idempotent on both ends: the relay applies the same policy onto the same scope.
func (s *SessionService) refreshInventoryPolicies(session *sessionState) {
	s.sendInventoryPolicies(session)
	if s.inventoryInterval <= 0 {
		return
	}
	ticker := time.NewTicker(s.inventoryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-session.ctx.Done():
			return
		case <-ticker.C:
			s.sendInventoryPolicies(session)
		}
	}
}

// sendInventoryPolicies opens one synchronization scope per Kubernetes evidence
// Connection this registration serves and asks the relay to watch each. Failure is
// logged and not fatal to the session: a relay that received no policy synchronizes
// nothing, which the scope's freshness surface reports, and the refresh retries.
func (s *SessionService) sendInventoryPolicies(session *sessionState) {
	scopes, err := s.database.OpenInventoryScopes(
		session.ctx, session.organization, session.registrationID, s.inventoryInterval)
	if err != nil {
		session.logger.ErrorContext(session.ctx, "opening inventory scopes",
			slog.String("error", err.Error()))
		return
	}
	for _, scope := range scopes {
		if err := send(session, inventoryPolicy(scope)); err != nil {
			session.logger.WarnContext(session.ctx, "an inventory policy could not be sent",
				slog.String("integration_id", scope.IntegrationID.String()),
				slog.String("error", err.Error()))
			return
		}
	}
}

// recordInventoryDelta records one delta and only then acknowledges it — record before
// acknowledge, exactly as results are handled, because an ack is the relay's licence to
// forget. A delta naming a Connection this relay does not serve is refused, logged and
// STILL acknowledged: the relay can do nothing to make it recordable, and resending it
// forever helps nobody.
func (s *SessionService) recordInventoryDelta(
	session *sessionState, delta *relayv1.InventoryDelta,
) error {
	ctx := session.ctx
	integrationID, err := uuid.Parse(delta.GetConnectionId())
	if err != nil {
		// Refused AND acknowledged, like a delta for an unserved Connection: nothing the
		// relay does can make this recordable, and an unacked refusal would be resent
		// forever.
		session.logger.WarnContext(ctx, "inventory delta names no connection",
			slog.String("delta_id", delta.GetDeltaId()))
		return send(session, inventoryDeltaAck(delta.GetDeltaId()))
	}

	reduced, skipped := reduceDelta(delta, integrationID)
	if skipped > 0 {
		// A change this build cannot read — an unknown kind from a newer relay, or an
		// identity outside the schema's bounds — is dropped rather than allowed to refuse
		// the whole delta. Dedup makes the drop safe: the rest records once, and what was
		// dropped stays observable to a build that understands it.
		session.logger.WarnContext(ctx, "inventory changes were not recordable by this build",
			slog.String("delta_id", delta.GetDeltaId()),
			slog.Int("skipped", skipped))
	}

	recorded, err := s.database.RecordInventoryDelta(
		ctx, session.organization, session.registrationID, reduced)
	if err != nil {
		// Nothing acknowledged, so the relay resends and the redelivery collapses
		// against whatever part was recorded.
		session.logger.ErrorContext(ctx, "recording an inventory delta",
			slog.String("error", err.Error()))
		return nil
	}
	if recorded.Refused {
		session.logger.WarnContext(ctx, "inventory delta refused: connection not served by this relay",
			slog.String("integration_id", integrationID.String()))
	}
	return send(session, inventoryDeltaAck(delta.GetDeltaId()))
}

// recordInventoryFreshness applies a heartbeat's scope stamps. A heartbeat with none is
// the common case and writes nothing.
func (s *SessionService) recordInventoryFreshness(
	session *sessionState, heartbeat *relayv1.Heartbeat,
) {
	scopes := heartbeat.GetInventoryScopes()
	if len(scopes) == 0 {
		return
	}
	stamps := make([]changeledger.Freshness, 0, len(scopes))
	for _, scope := range scopes {
		integrationID, err := uuid.Parse(scope.GetConnectionId())
		if err != nil {
			continue
		}
		stamp := changeledger.Freshness{
			IntegrationID:  integrationID,
			PolicyRevision: int64(scope.GetPolicyRevision()),
			Faulted:        scope.GetFaulted(),
			Truncated:      scope.GetTruncated(),
		}
		if completed := scope.GetLastCompletedAt(); completed != nil {
			at := completed.AsTime()
			stamp.CompletedAt = &at
		}
		stamps = append(stamps, stamp)
	}
	if err := s.database.RecordInventoryFreshness(
		session.ctx, session.organization, session.registrationID, stamps); err != nil {
		session.logger.WarnContext(session.ctx, "recording inventory freshness",
			slog.String("error", err.Error()))
	}
}

// reduceDelta converts a wire delta to domain terms, dropping what this build cannot
// record and counting the drops.
func reduceDelta(
	delta *relayv1.InventoryDelta, integrationID uuid.UUID,
) (changeledger.Delta, int) {
	reduced := changeledger.Delta{
		IntegrationID:  integrationID,
		PolicyRevision: int64(delta.GetPolicyRevision()),
		Baseline:       delta.GetBaseline(),
		ObservedAt:     delta.GetObservedAt().AsTime(),
	}
	skipped := 0
	for _, change := range delta.GetChanges() {
		domain, ok := reduceChange(change)
		if !ok {
			skipped++
			continue
		}
		reduced.Changes = append(reduced.Changes, domain)
	}
	return reduced, skipped
}

func reduceChange(change *relayv1.InventoryObjectChange) (changeledger.Change, bool) {
	kind, ok := objectKindOf(change.GetKind())
	if !ok {
		return changeledger.Change{}, false
	}
	changeKind, ok := changeKindOf(change.GetChange())
	if !ok {
		return changeledger.Change{}, false
	}
	// The schema's own bounds, checked here so one out-of-bounds change costs itself and
	// not the transaction the rest of the delta records in.
	if !within(change.GetNamespace(), 1, 63) || !within(change.GetName(), 1, 253) ||
		!within(change.GetUid(), 1, 128) || !within(change.GetObservedRevision(), 0, 128) {
		return changeledger.Change{}, false
	}
	if (changeKind == changeledger.ChangeDeleted) != (change.GetObservedRevision() == "") {
		return changeledger.Change{}, false
	}

	domain := changeledger.Change{
		Namespace:        change.GetNamespace(),
		Kind:             kind,
		Name:             change.GetName(),
		UID:              change.GetUid(),
		ObservedRevision: change.GetObservedRevision(),
		Change:           changeKind,
	}
	for _, field := range change.GetFields() {
		if !within(field.GetField(), 1, 253) ||
			!within(field.GetBefore(), 0, 512) || !within(field.GetAfter(), 0, 512) {
			return changeledger.Change{}, false
		}
		domain.Fields = append(domain.Fields, changeledger.FieldChange{
			Field:  field.GetField(),
			Before: field.GetBefore(),
			After:  field.GetAfter(),
		})
	}
	return domain, true
}

func objectKindOf(kind relayv1.InventoryObjectKind) (changeledger.ObjectKind, bool) {
	switch kind {
	case relayv1.InventoryObjectKind_INVENTORY_OBJECT_KIND_DEPLOYMENT:
		return changeledger.KindDeployment, true
	case relayv1.InventoryObjectKind_INVENTORY_OBJECT_KIND_STATEFULSET:
		return changeledger.KindStatefulSet, true
	case relayv1.InventoryObjectKind_INVENTORY_OBJECT_KIND_DAEMONSET:
		return changeledger.KindDaemonSet, true
	case relayv1.InventoryObjectKind_INVENTORY_OBJECT_KIND_CONFIGMAP:
		return changeledger.KindConfigMap, true
	case relayv1.InventoryObjectKind_INVENTORY_OBJECT_KIND_SECRET:
		return changeledger.KindSecret, true
	default:
		return 0, false
	}
}

func changeKindOf(kind relayv1.InventoryChangeKind) (changeledger.ChangeKind, bool) {
	switch kind {
	case relayv1.InventoryChangeKind_INVENTORY_CHANGE_KIND_CREATED:
		return changeledger.ChangeCreated, true
	case relayv1.InventoryChangeKind_INVENTORY_CHANGE_KIND_MODIFIED:
		return changeledger.ChangeModified, true
	case relayv1.InventoryChangeKind_INVENTORY_CHANGE_KIND_DELETED:
		return changeledger.ChangeDeleted, true
	default:
		return 0, false
	}
}

func within(value string, min, max int) bool {
	return len(value) >= min && len(value) <= max
}
