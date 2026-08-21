package investigation

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// THE LEASE — an investigation is work somebody holds, not a goroutine somebody hopes for.
//
// Claiming a lease is how a worker says "I am doing this", the heartbeat is how it keeps
// saying so, and an expiry the SERVER's clock decides is how everybody else learns it
// stopped. Two things depend on it: a record that says `running` is always either claimable
// or genuinely being worked on, and two replicas cannot both take one conversation's turn.
//
// The shape is relay_job's, deliberately: that is the one place in this codebase where
// "never lost, never done twice" is already proven under server-clock leases, and a
// reviewer reading this will recognise it.
//
// NO NEW STATUS VALUE. An investigation with no lease is waiting to be claimed; one with a
// live lease is executing. Both are `running`, because both are true, and the first stream
// event says which.

const (
	// heartbeatInterval is how often a working claimer renews its lease. Short relative
	// to the lease, so several heartbeats have to be missed before anything is swept.
	heartbeatInterval = 2 * time.Minute
	// leaseDuration is how long a claim survives without a heartbeat. Comfortably longer
	// than one reasoner turn, because a worker thinking for six minutes has not stopped.
	leaseDuration = 15 * time.Minute
	// claimInterval is how often an idle worker looks for work. A turn opened by a
	// message should start within a second of being asked for, and this is a
	// primary-key-ordered index scan.
	claimInterval = 500 * time.Millisecond
	// sweepInterval is how often lapsed leases are recovered. A crashed worker's
	// investigation is already stale by the time it is swept; checking more often would
	// not make it fresher.
	sweepInterval = time.Minute
	// sweepBatch bounds one recovery pass, so a crash loop that stranded hundreds does not
	// produce one enormous transaction.
	sweepBatch = 50
)

// RecoveryReason is what a swept investigation records as its failure, and what the reader
// watching it is finally told.
//
// It is a FAILURE and not a resumption. Resuming would be a fiction: the model transcript
// is deliberately not persisted — only semantic events are — so there is nothing to
// continue from, and inventing a continuation would produce an answer nobody's reads
// support. An honest failure the operator can re-ask from is the better of the two.
const RecoveryReason = "the worker running this investigation stopped"

// Claim is what a worker asks for when it takes work.
type Claim struct {
	// Worker identifies this process. It only has to be unique among live workers; a
	// duplicate would mean two processes could renew each other's leases.
	Worker   string
	LeaseFor time.Duration
	// OrgConcurrent is the most investigations one organization may have executing at
	// once, so a single tenant cannot consume the whole deployment.
	OrgConcurrent int
}

// Leases is the durable half of claiming. It is placement-wide rather than
// organization-scoped, because a worker looks for work across the tenants it serves and
// does not know which one will have some.
type Leases interface {
	// ClaimInvestigation leases the oldest waiting investigation a worker may take, and
	// reports the organization it belongs to. The boolean is false when there is nothing
	// to claim, which is the ordinary case.
	ClaimInvestigation(ctx context.Context, claim Claim) (
		tenancy.Organization, Investigation, bool, error)
	// TakeLease claims one NAMED investigation, for the path that already holds the
	// record it just created. False means somebody else has it.
	TakeLease(ctx context.Context, org tenancy.Organization, id uuid.UUID,
		claim Claim) (bool, error)
	// Heartbeat renews a lease this worker holds. False means the lease is gone — swept,
	// or the investigation already ended — and the holder must stop.
	Heartbeat(ctx context.Context, org tenancy.Organization, id uuid.UUID,
		claim Claim) (bool, error)
	// RecoverStale fails every investigation whose lease lapsed, writing each a terminal
	// event, and reports how many. Bounded per call.
	RecoverStale(ctx context.Context, reason string, limit int) (int, error)
}
