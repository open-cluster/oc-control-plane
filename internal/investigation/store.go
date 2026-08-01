package investigation

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// Store is everything the investigator needs from durable state. It is declared here rather than
// in the persistence package because the capability owns its vocabulary and persistence depends on
// it (ADR-017) — the dependency points this way, and an import cycle in the other direction would
// be a sign that a type is owned by the wrong capability.
//
// Every method takes the organization explicitly. An ambient tenant is how one customer is served
// another customer's rows, and the property is invisible in review.
//
// Every write inside a round takes a Fence. The fence is what stops a worker whose lease expired
// from writing to the run that outlived it: the write matches nothing and reports ErrLeaseLost
// rather than succeeding into a round another execution now owns.
type Store interface {
	// OpenInvestigation opens a case. The Environment is DERIVED from the Connection inside the
	// write, so a caller cannot assert one and a Connection disabled between a check and the
	// insert cannot leave a case open against it.
	OpenInvestigation(ctx context.Context, org tenancy.Organization, wanted New) (Investigation, error)
	// Investigation reads one case, scoped to the tenant.
	Investigation(ctx context.Context, org tenancy.Organization, id uuid.UUID) (Investigation, error)
	// CancelInvestigation stops a case and its running round. A cancelled case is terminal and
	// dispatches nothing further.
	CancelInvestigation(ctx context.Context, org tenancy.Organization, id uuid.UUID) error

	// OpenRound adds a bounded execution to a case, pinning what it will run under.
	OpenRound(ctx context.Context, org tenancy.Organization, opening Opening) (Round, error)
	// ClaimRounds leases work for one worker session. Expired leases are claimable again and every
	// claim raises the generation, so a write from the execution that lost its lease is refused.
	ClaimRounds(ctx context.Context, org tenancy.Organization, claim RoundClaim) ([]Claimed, error)
	// RenewRoundLease extends a held lease. It is deliberately NOT a change within the case: a
	// heartbeat must not wake every polling client.
	RenewRoundLease(ctx context.Context, org tenancy.Organization, fence Fence, extend time.Duration) error
	// FinishRound stamps a round terminal and projects its outcome onto the case.
	FinishRound(ctx context.Context, org tenancy.Organization, fence Fence, finish Finish) error

	// RecordBrief pins the deterministic orientation and moves the case out of briefing. It is
	// written before any hypothesis exists, which is what makes that ordering observable.
	RecordBrief(ctx context.Context, org tenancy.Organization, fence Fence, brief Brief) error
	// AdvanceLifecycle moves the case between the non-terminal execution states.
	AdvanceLifecycle(ctx context.Context, org tenancy.Organization, fence Fence, to Lifecycle) error

	// RecordHypotheses appends what the planner proposed and returns them with their identities.
	RecordHypotheses(ctx context.Context, org tenancy.Organization, fence Fence, proposed []Hypothesis) ([]Hypothesis, error)
	// SettleHypothesis moves one hypothesis's state, with the reason when it is set aside.
	SettleHypothesis(ctx context.Context, org tenancy.Organization, fence Fence, settled Hypothesis) error
	// RecordRequest appends one proposed read in whatever state validation left it. A refused
	// request is recorded and never dispatched.
	RecordRequest(ctx context.Context, org tenancy.Organization, fence Fence, request Request) (Request, error)
	// SettleRequest records what a dispatched read came to.
	SettleRequest(ctx context.Context, org tenancy.Organization, fence Fence, request Request) error
	// RecordEvidence appends validated items and returns them with their identities and ordinals.
	RecordEvidence(ctx context.Context, org tenancy.Organization, fence Fence, items []Item) ([]Item, error)
	// RecordGaps appends coverage gaps and returns them with their identities and ordinals.
	RecordGaps(ctx context.Context, org tenancy.Organization, fence Fence, gaps []Gap) ([]Gap, error)
	// RecordStances records how evidence stands towards hypotheses.
	RecordStances(ctx context.Context, org tenancy.Organization, fence Fence, weighed []Weighed) error
	// RecordCoverage records per-capability readiness for this round.
	RecordCoverage(ctx context.Context, org tenancy.Organization, fence Fence, coverage []Coverage) error
	// RecordOutcome writes an outcome and its cited claims in one transaction, superseding the
	// case's previous outcome. Half of it committing would leave a claim with no citations, which
	// is the one shape the truth model may not produce.
	RecordOutcome(ctx context.Context, org tenancy.Organization, fence Fence, outcome Outcome) error
	// RecordSpend adds what a pass consumed to the round and to the case.
	RecordSpend(ctx context.Context, org tenancy.Organization, fence Fence, spend Spend) error

	// Dispatch enqueues one typed read against the case's Connection and returns the job it
	// became. The Connection is a precondition of the write rather than a value copied in, so a
	// read against a Connection that cannot carry it is refused by the database.
	Dispatch(ctx context.Context, org tenancy.Organization, sending Sending) (uuid.UUID, error)
	// Reads reports what dispatched jobs have come to.
	Reads(ctx context.Context, org tenancy.Organization, jobs []uuid.UUID) ([]Read, error)

	// CasePack reads one round's closed-world record, sufficient to replay it with no access to
	// live sources or current state.
	CasePack(ctx context.Context, org tenancy.Organization, roundID uuid.UUID) (CasePack, error)
}

// ErrLeaseLost reports a write from an execution that no longer owns its round. Nothing was
// written. It is not an error in the sense of something being broken: a control-plane restart
// produces it by design, and the correct answer is to stop, not to retry.
var ErrLeaseLost = errors.New("the round's lease is held by another execution")

// ErrRoundUnknown reports a round this organization does not have.
var ErrRoundUnknown = errors.New("investigation round unknown")

// Opening is a round about to be added to a case, with everything it will run under pinned.
type Opening struct {
	InvestigationID uuid.UUID
	Controls        Controls
	Plan            Plan
	Versions        Versions
}

// RoundClaim is what one worker session asks for when it takes work.
type RoundClaim struct {
	SessionID uuid.UUID
	LeaseFor  time.Duration
	// Capacity is the most rounds this session may hold at once, not the most it may take in one
	// call. What it already holds is subtracted before anything more is leased.
	Capacity int
}

// Claimed is a round a worker now owns, with the case it belongs to.
type Claimed struct {
	Investigation Investigation
	Round         Round
}

// Fence is what a claimed round hands to every write it makes.
func (c Claimed) Fence() Fence {
	return Fence{
		RoundID:      c.Round.ID,
		LeaseSession: c.Round.LeaseSession,
		LeaseEpoch:   c.Round.LeaseEpoch,
	}
}

// Finish is how a round ended.
type Finish struct {
	Outcome RoundOutcome
	// Exhausted names the bound that ran out, when one did. Zero means none did.
	Exhausted Limit
}

// Sending is one typed read about to be dispatched. It has already been validated: giving an
// unvalidated read and a dispatched one the same type is how the unvalidated one reaches a
// customer's cluster.
type Sending struct {
	InvestigationID   uuid.UUID
	ConnectionID      uuid.UUID
	CapabilityID      string
	CapabilityVersion uint32
	Arguments         []byte
}

// Read is what a dispatched capability read came back as.
type Read struct {
	JobID uuid.UUID
	// Settled reports whether the job reached a terminal state. An unsettled read is still running
	// and is neither a success nor a failure.
	Settled bool
	// Succeeded distinguishes a result from a failure. A cancelled or failed job is settled and
	// did not succeed.
	Succeeded bool
	Result    []byte
}

// CasePack is one round's immutable closed-world record: its inputs, evidence, gaps, hypotheses,
// outcome, execution controls and component versions. It is sufficient to replay that round with
// no access to live sources or current state, which is the property that makes a surprising
// conclusion examinable rather than arguable.
type CasePack struct {
	Investigation Investigation
	Round         Round
	Hypotheses    []Hypothesis
	Requests      []Request
	Evidence      []Item
	Gaps          []Gap
	Stances       []Weighed
	Coverage      []Coverage
	// Outcome is nil for a round that has not reached one.
	Outcome *Outcome
}
