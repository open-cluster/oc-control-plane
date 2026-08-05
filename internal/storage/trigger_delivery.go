package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// What has actually arrived through a trigger Connection, accepted or not.
//
// signal_delivery records what was ACCEPTED and is the idempotence key that makes an
// at-least-once webhook safe; it must keep doing exactly that. This is the other half. Without
// it, a source that is delivering every thirty seconds and being rejected every time looks
// identical to a source that has gone quiet — and those two call for opposite actions from
// whoever is awake at three in the morning.

// DeliveryDisposition is what happened to one delivery attempt.
type DeliveryDisposition int16

const (
	// DeliveryAccepted is a delivery that was recorded and applied.
	DeliveryAccepted DeliveryDisposition = iota + 1
	// DeliveryDuplicate is a body already accepted through this Connection. It is neither a
	// success to celebrate nor a rejection to investigate: a source retrying because it never
	// saw a response has done nothing wrong, and counting those as rejections would make a
	// healthy retry look like a broken signature.
	DeliveryDuplicate
	DeliveryRejected
)

func (d DeliveryDisposition) String() string {
	switch d {
	case DeliveryAccepted:
		return "accepted"
	case DeliveryDuplicate:
		return "duplicate"
	case DeliveryRejected:
		return "rejected"
	default:
		return "unrecognised"
	}
}

// The reasons a delivery is refused. They are a closed set of short tokens rather than free
// text, because they are COUNTED — "how many were rejected and why" is the question — and a
// count grouped by a sentence somebody phrased slightly differently twice is two counts.
const (
	RefusedUnauthenticated = "unauthenticated"
	RefusedDisabled        = "disabled"
	RefusedNoTriggerRole   = "no_trigger_role"
	RefusedMalformed       = "malformed"
	RefusedOversized       = "oversized"
	RefusedRateLimited     = "rate_limited"
	RefusedIncomplete      = "incomplete"
)

// DeliveryAttempt is one attempt as it is recorded. It carries nothing from the payload and
// nothing from the headers: the body is untrusted text from a customer's systems, and the
// headers of a refused delivery are the one place guaranteed to hold a guess at the credential.
type DeliveryAttempt struct {
	Connection  uuid.UUID
	Disposition DeliveryDisposition
	// Reason is required for a rejection and empty otherwise. The schema enforces both
	// directions, so a rejection with no reason cannot be written.
	Reason      string
	SignalCount int
	// Test marks an operator's own probe. A test indistinguishable from a real delivery would
	// make "intake is working" a claim resting on a delivery this platform sent itself.
	Test bool
}

// RecordDeliveryAttempt writes one attempt.
//
// It takes no principal because intake has none: the caller is a customer's own system, and
// what authenticates it is the Connection's secret rather than a person. It takes the
// organization because the row that authenticated already carries one — discovered from the
// Connection, never claimed by the caller.
//
// WHAT BOUNDS THIS, since it is reachable by anything that can guess an identifier: the intake
// surface's per-Connection rate limiter runs first, and an attempt naming an identifier that
// resolves to no Connection is never recorded at all. So the write rate is bounded per existing
// Connection, and an attacker with random identifiers writes nothing.
func (p *Placements) RecordDeliveryAttempt(
	ctx context.Context, organization tenancy.Organization, attempt DeliveryAttempt,
) error {
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}
	if _, err = pool.Exec(ctx, `
		INSERT INTO trigger_delivery
			(delivery_attempt_id, organization, connection_id, outcome, reason, signal_count,
			 is_test)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		uuid.New(), organization.String(), attempt.Connection, int16(attempt.Disposition),
		attempt.Reason, attempt.SignalCount, attempt.Test); err != nil {
		return fmt.Errorf("recording a delivery attempt: %w", err)
	}
	return nil
}

// TriggerHealth is what an operator needs to tell a quiet night from a broken intake.
//
// LastReceivedAt and LastAcceptedAt are SEPARATE and that is the whole point. One value would
// hide the case this exists for: a source delivering every thirty seconds and being rejected
// every time has a recent LastReceivedAt and an ancient LastAcceptedAt, and any single "last
// delivery" field would report whichever of those the implementer happened to choose.
type TriggerHealth struct {
	// Observed reports that this Connection has a delivery history at all. False means nothing
	// has ever been attempted through it — which is a fact, and is different from a source that
	// has stopped.
	Observed       bool
	LastReceivedAt time.Time
	LastAcceptedAt time.Time
	Accepted       int
	Duplicates     int
	Rejected       int
	// RejectionsByReason counts refusals by why, so "fix a signature or a clock skew" is a
	// decision an operator can make from the summary rather than from reading the history.
	RejectionsByReason map[string]int
	// Window is how far back these counts reach. It is stated rather than assumed, because a
	// count with no window is a number nobody can compare to anything.
	Window time.Duration
}

// deliveryHealthWindow is how far back the counts reach. Seven days: long enough that a source
// that broke over a weekend is still visible, short enough that the count means something about
// now rather than about the Connection's whole life.
const deliveryHealthWindow = 7 * 24 * time.Hour

// TriggerDeliveryHealth reads one Connection's intake health.
func (p *Placements) TriggerDeliveryHealth(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	id uuid.UUID,
) (TriggerHealth, error) {
	if !principal.MemberOf(organization) {
		return TriggerHealth{}, ErrNotAMember
	}
	pool, err := p.Pool(organization)
	if err != nil {
		return TriggerHealth{}, err
	}

	health := TriggerHealth{
		RejectionsByReason: map[string]int{},
		Window:             deliveryHealthWindow,
	}
	var lastReceived, lastAccepted *time.Time
	err = pool.QueryRow(ctx, `
		SELECT max(received_at),
		       max(received_at) FILTER (WHERE outcome = 1),
		       count(*) FILTER (WHERE outcome = 1),
		       count(*) FILTER (WHERE outcome = 2),
		       count(*) FILTER (WHERE outcome = 3),
		       count(*) > 0
		  FROM trigger_delivery
		 WHERE organization = $1 AND connection_id = $2
		   AND received_at > now() - $3::interval`,
		organization.String(), id, deliveryHealthWindow).
		Scan(&lastReceived, &lastAccepted, &health.Accepted, &health.Duplicates,
			&health.Rejected, &health.Observed)
	if err != nil {
		return TriggerHealth{}, fmt.Errorf("reading a trigger's delivery health: %w", err)
	}
	if lastReceived != nil {
		health.LastReceivedAt = *lastReceived
	}
	if lastAccepted != nil {
		health.LastAcceptedAt = *lastAccepted
	}

	rows, err := pool.Query(ctx, `
		SELECT reason, count(*)
		  FROM trigger_delivery
		 WHERE organization = $1 AND connection_id = $2 AND outcome = 3
		   AND received_at > now() - $3::interval
		 GROUP BY reason
		 ORDER BY reason`,
		organization.String(), id, deliveryHealthWindow)
	if err != nil {
		return TriggerHealth{}, fmt.Errorf("counting refused deliveries: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			reason string
			count  int
		)
		if err = rows.Scan(&reason, &count); err != nil {
			return TriggerHealth{}, fmt.Errorf("counting refused deliveries: %w", err)
		}
		health.RejectionsByReason[reason] = count
	}
	if err = rows.Err(); err != nil {
		return TriggerHealth{}, fmt.Errorf("counting refused deliveries: %w", err)
	}
	return health, nil
}

// RecordedDelivery is one attempt as it is read back.
type RecordedDelivery struct {
	ID          uuid.UUID
	Disposition DeliveryDisposition
	Reason      string
	SignalCount int
	Test        bool
	ReceivedAt  time.Time
}

// DeliveryList is a page of a Connection's delivery history, newest first.
type DeliveryList struct {
	Deliveries []RecordedDelivery
	Next       string
}

// DeliveryQuery is what a caller may narrow and order a delivery history by. Both are applied by
// the database, so a page holds what it says it holds.
type DeliveryQuery struct {
	Page Page
	// Outcome narrows to accepted, duplicate or rejected. Zero means every outcome.
	Outcome DeliveryDisposition
	// Descending is newest first, which is the default an incident is read in.
	Descending bool
}

// nullableDisposition renders "no narrowing" as SQL NULL, so one statement serves both the
// filtered and the unfiltered read rather than two that can drift apart.
func nullableDisposition(outcome DeliveryDisposition) *int16 {
	if outcome == 0 {
		return nil
	}
	value := int16(outcome)
	return &value
}

// TriggerDeliveries reads a Connection's delivery history, so a missed investigation can be
// correlated with a missed delivery rather than guessed at.
func (p *Placements) TriggerDeliveries(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	id uuid.UUID, query DeliveryQuery,
) (DeliveryList, error) {
	if !principal.MemberOf(organization) {
		return DeliveryList{}, ErrNotAMember
	}
	pool, err := p.Pool(organization)
	if err != nil {
		return DeliveryList{}, err
	}
	limit := pageLimit(query.Page.Limit)
	after, afterID, err := decodeCursor(query.Page.After)
	if err != nil {
		return DeliveryList{}, err
	}

	// The outcome narrowing is part of the QUERY rather than applied to the rows afterwards. It
	// was applied afterwards, and that is a page which returns fewer rows than it was asked for
	// with a `next` that points past what it dropped — a filter the backend appears to honour
	// and does not.
	//
	// Ascending is served as well as descending. A listing that advertises a sortable field and
	// then hardcodes one direction is the same silent lie an unknown sort field is refused to
	// prevent: the caller asks, gets a 200, and is given an order nobody chose.
	direction := "DESC"
	comparison := "<"
	if !query.Descending {
		direction, comparison = "ASC", ">"
	}
	rows, err := pool.Query(ctx, fmt.Sprintf(`
		SELECT delivery_attempt_id, outcome, reason, signal_count, is_test, received_at
		  FROM trigger_delivery
		 WHERE organization = $1 AND connection_id = $2
		   AND ($6::smallint IS NULL OR outcome = $6::smallint)
		   AND ($4::timestamptz IS NULL
		        OR (received_at, delivery_attempt_id) %s ($4::timestamptz, $5::uuid))
		 ORDER BY received_at %s, delivery_attempt_id %s
		 LIMIT $3`, comparison, direction, direction),
		organization.String(), id, limit+1, after, afterID, nullableDisposition(query.Outcome))
	if err != nil {
		return DeliveryList{}, fmt.Errorf("reading a connection's deliveries: %w", err)
	}
	defer rows.Close()

	list := DeliveryList{Deliveries: make([]RecordedDelivery, 0, limit)}
	for rows.Next() {
		var (
			delivery    RecordedDelivery
			disposition int16
		)
		if err = rows.Scan(&delivery.ID, &disposition, &delivery.Reason, &delivery.SignalCount,
			&delivery.Test, &delivery.ReceivedAt); err != nil {
			return DeliveryList{}, fmt.Errorf("reading a delivery: %w", err)
		}
		if len(list.Deliveries) == limit {
			last := list.Deliveries[limit-1]
			list.Next = encodeCursor(last.ReceivedAt, last.ID)
			break
		}
		delivery.Disposition = DeliveryDisposition(disposition)
		list.Deliveries = append(list.Deliveries, delivery)
	}
	if err = rows.Err(); err != nil {
		return DeliveryList{}, fmt.Errorf("reading a connection's deliveries: %w", err)
	}
	return list, nil
}

// RecordTestDelivery records an operator's own probe of a trigger Connection, and writes the
// audit event that says who sent it.
//
// It is audited where an ordinary delivery is not, because an ordinary delivery has no operator
// behind it and this one does. A row in the delivery history that nobody can attribute would be
// indistinguishable from one the customer's own system sent, which would make the history a
// worse record than it was before test events existed.
func (p *Placements) RecordTestDelivery(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	id uuid.UUID,
) error {
	_, err := audited(ctx, p, principal, organization, audit.ActionTriggerTested,
		func(ctx context.Context, transaction pgx.Tx) (struct{}, audit.Target, audit.Detail, error) {
			// Guarded on the Connection being one that could actually receive a delivery. A test
			// that "succeeded" against a disabled or evidence-only Connection would report
			// working intake for a Connection that refuses every real delivery.
			tag, err := transaction.Exec(ctx, `
				INSERT INTO trigger_delivery
					(delivery_attempt_id, organization, connection_id, outcome, signal_count,
					 is_test)
				SELECT $1, $2, $3, 1, 0, TRUE
				  FROM connection
				 WHERE organization = $2 AND connection_id = $3
				   AND role IN (1, 3) AND disabled_at IS NULL`,
				uuid.New(), organization.String(), id)
			if err != nil {
				return struct{}{}, audit.Target{}, nil,
					fmt.Errorf("recording a test delivery: %w", err)
			}
			if tag.RowsAffected() == 0 {
				return struct{}{}, audit.Target{}, nil, ErrConnectionUnknown
			}
			return struct{}{},
				audit.Target{Kind: audit.TargetConnection, ID: id.String()},
				audit.Detail{
					"effect": "a test delivery was recorded in this connection's history, " +
						"marked as a test",
				}, nil
		})
	return err
}
