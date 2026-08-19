// Package investigation owns the investigation domain: opening one from an alert episode
// or an operator's question, routing it to a small ranked set of connected sources,
// running bounded read tools against them, and persisting OPERATIONAL PROVENANCE — what
// was triggered, which integrations were queried and why, which tools ran over which
// window, what each returned or failed with, and the findings with their sources.
//
// What is deliberately NOT here is an AI chain of thought. No hypothesis, no evidence
// stance, no coverage certificate is persisted: the reasoner's working is its own, and
// what an operator audits is what was actually done and found.
//
// The model boundary is declared here and implemented elsewhere; this package never
// imports a reasoning package and never learns that a vendor exists.
package investigation

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// Status is where an investigation has got to. Persisted as the integer in the column;
// the values are frozen.
type Status int16

const (
	StatusRunning Status = iota + 1
	StatusConcluded
	StatusFailed
)

// The ceilings that can force a concluding turn, as stopped_by records them. Persisted
// text, frozen like an enum: the operator view and its clients key on these words.
// Empty means the model concluded freely. Wall clock and stagnation are the autonomous
// loop's own: the deterministic loop cannot reach them.
const (
	StoppedBySpend         = "spend"
	StoppedByToolRuns      = "tool_runs"
	StoppedByReasonerTurns = "reasoner_turns"
	StoppedByWallClock     = "wall_clock"
	StoppedByStagnation    = "stagnation"
)

func (s Status) String() string {
	switch s {
	case StatusRunning:
		return "running"
	case StatusConcluded:
		return "concluded"
	case StatusFailed:
		return "failed"
	default:
		return "unrecognised"
	}
}

// RunOutcome is how one tool execution ended. Persisted; the values are frozen.
type RunOutcome int16

const (
	RunSucceeded RunOutcome = iota + 1
	RunFailed
)

// Refusals and failures this capability names.
var (
	// ErrUnknown reports an investigation this organization does not have.
	ErrUnknown = errors.New("investigation unknown")
	// ErrEpisodeUnknown reports an episode this organization does not have.
	ErrEpisodeUnknown = errors.New("episode unknown")
	// ErrReasonerUnavailable is the domain's word for the model boundary failing: the
	// reasoning step could not run. Every named provider failure wraps it, and the
	// investigation it ends is FAILED — never concluded, because a conclusion nobody
	// reasoned towards is worse than an honest failure.
	ErrReasonerUnavailable = errors.New("the reasoning boundary is unavailable")
	// ErrBadCursor reports a page position that did not come from a previous response.
	ErrBadCursor = errors.New("after is not a page position from a previous response")
)

// Finding is one thing the investigation established, tied to the runs that support it.
type Finding struct {
	Statement string
	// Kind is the finding's causal role — FindingProbableCause and its siblings. Empty
	// on a deterministic-loop finding, which predates the vocabulary; required from the
	// autonomous conclusion, enforced where it is decoded.
	Kind string
	// Confidence is categorical — confirmed, likely, possible — never an invented
	// numeric certainty. Empty exactly when Kind is.
	Confidence string
	// Sources are one-based ordinals among the investigation's recorded tool runs. Every
	// finding cites at least one — enforced when the reasoner's answer is decoded — so a
	// statement nothing was read for cannot be stored as established.
	Sources []int
}

// The causal kinds a finding distinguishes: persisted inside the findings record,
// frozen like an enum. Multiple probable causes are legal — no incident is forced into
// a single-cause model.
const (
	FindingProbableCause      = "probable_cause"
	FindingContributingFactor = "contributing_factor"
	FindingSymptom            = "symptom"
	FindingTriggeringChange   = "triggering_change"
	FindingPropagationEffect  = "propagation_effect"
	FindingRuledOut           = "ruled_out"
	FindingUnresolvedLead     = "unresolved_lead"
)

// The categorical confidence vocabulary. Persisted, frozen.
const (
	ConfidenceConfirmed = "confirmed"
	ConfidenceLikely    = "likely"
	ConfidencePossible  = "possible"
)

// FindingKinds and Confidences enumerate the legal values, for decoders and gates.
var (
	FindingKinds = []string{
		FindingProbableCause, FindingContributingFactor, FindingSymptom,
		FindingTriggeringChange, FindingPropagationEffect, FindingRuledOut,
		FindingUnresolvedLead,
	}
	Confidences = []string{ConfidenceConfirmed, ConfidenceLikely, ConfidencePossible}
)

// Spend is what the reasoning behind an investigation consumed.
type Spend struct {
	InputTokens  int64
	OutputTokens int64
	// MicroCents is integer money, summed from the priced calls.
	MicroCents int64
}

// Add accumulates another call's spend.
func (s Spend) Add(other Spend) Spend {
	return Spend{
		InputTokens:  s.InputTokens + other.InputTokens,
		OutputTokens: s.OutputTokens + other.OutputTokens,
		MicroCents:   s.MicroCents + other.MicroCents,
	}
}

// Investigation is the slim record: the trigger, the subject, the window, the lifecycle,
// what it found and what it cost. Everything else an auditor needs is the provenance
// beside it.
type Investigation struct {
	ID    uuid.UUID
	OrgID string
	// EpisodeID is the alert episode that triggered this, zero for a question with no
	// episode resolved. IntegrationID is the episode's originating integration.
	EpisodeID     uuid.UUID
	IntegrationID uuid.UUID
	// Question is the operator's own words, empty for an alert-triggered investigation.
	Question string
	// Subject is what is being investigated, in plain language.
	Subject     string
	WindowFrom  time.Time
	WindowUntil time.Time
	Status      Status
	Findings    []Finding
	// NextSteps are the conclusion's recommended actions, in the order the operator
	// should consider them; empty when the conclusion carried none.
	NextSteps []string
	// StoppedBy names the ceiling that forced the concluding turn — StoppedBySpend and
	// its siblings — and is empty when the model concluded freely. A stopped
	// investigation still concluded, with what was established and what remains
	// unresolved; the label is what keeps resource exhaustion from being rendered as a
	// completed diagnosis.
	StoppedBy string
	// Error says why a failed investigation failed, in the operator's language.
	Error       string
	Spend       Spend
	CreatedBy   string
	CreatedAt   time.Time
	ConcludedAt time.Time
}

// Source is one integration the router selected, with the reason it was.
type Source struct {
	IntegrationID uuid.UUID
	Rank          int
	// Reason says why this source joined the investigation — its score's terms, or the
	// expansion that pulled it in.
	Reason     string
	SelectedAt time.Time
}

// ToolRun is one tool execution: the scope it ran under and what came of it. Its
// identity is (investigation, ordinal) — the ordinal findings cite — not a surrogate id.
type ToolRun struct {
	IntegrationID uuid.UUID
	// Ordinal is the run's one-based position in the investigation, which is what a
	// finding cites.
	Ordinal    int
	Capability string
	Tool       string
	// Arguments is the call's scope as it ran, never carrying a credential.
	Arguments   map[string]any
	WindowFrom  time.Time
	WindowUntil time.Time
	Outcome     RunOutcome
	Truncated   bool
	// Summary is the provider's own one-line account of what came back; Sources are the
	// identifiers of what was read — channel ids, repository ids — so a finding citing
	// this run can be followed to its origin.
	Summary string
	Sources []string
	// Error is why a failed run failed.
	Error      string
	StartedAt  time.Time
	FinishedAt time.Time
	// Content is the run's full answer, held for the reasoner within the running
	// investigation and never persisted: provenance records what was found, not a copy
	// of everything read.
	Content any
}

// NewInvestigation is what a create records.
type NewInvestigation struct {
	EpisodeID     uuid.UUID
	IntegrationID uuid.UUID
	Question      string
	Subject       string
	WindowFrom    time.Time
	WindowUntil   time.Time
	CreatedBy     string
}

// Trigger is what an episode contributes when it starts an investigation. Annotations
// and GeneratorURL are the alert's own operational knowledge — runbook and dashboard
// links, the source's graph — preserved through intake and rendered in the autonomous
// orientation.
type Trigger struct {
	EpisodeID     uuid.UUID
	IntegrationID uuid.UUID
	Title         string
	Labels        map[string]string
	Annotations   map[string]string
	GeneratorURL  string
	FirstSeenAt   time.Time
	LastSeenAt    time.Time
	Resolved      bool
}

// Page is a position in a listing.
type Page struct {
	Limit int
	After string
}

// List is a page of an organization's investigations, newest first.
type List struct {
	Investigations []Investigation
	Next           string
}

// Store is everything this capability needs from durable state.
type Store interface {
	// CreateInvestigation records one, born running.
	CreateInvestigation(ctx context.Context, who authz.Principal, org tenancy.Organization,
		wanted NewInvestigation) (Investigation, error)
	// Investigation reads one, scoped to the tenant.
	Investigation(ctx context.Context, org tenancy.Organization,
		id uuid.UUID) (Investigation, error)
	// InvestigationProvenance reads the sources and runs beside one investigation.
	InvestigationProvenance(ctx context.Context, org tenancy.Organization,
		id uuid.UUID) ([]Source, []ToolRun, error)
	// QueryInvestigations reports a page, newest first.
	QueryInvestigations(ctx context.Context, who authz.Principal, org tenancy.Organization,
		page Page) (List, error)
	// RecordSource writes one router selection.
	RecordSource(ctx context.Context, org tenancy.Organization, id uuid.UUID,
		source Source) error
	// RecordToolRun writes one execution as it finished.
	RecordToolRun(ctx context.Context, org tenancy.Organization, id uuid.UUID,
		run ToolRun) error
	// ConcludeInvestigation ends one with its findings and spend. stoppedBy names the
	// ceiling that forced the concluding turn, empty when the model concluded freely;
	// nextSteps are the conclusion's recommended actions, nil when the conclusion
	// carries none.
	ConcludeInvestigation(ctx context.Context, org tenancy.Organization, id uuid.UUID,
		findings []Finding, nextSteps []string, stoppedBy string, spend Spend) error
	// FailInvestigation ends one with the reason it could not conclude.
	FailInvestigation(ctx context.Context, org tenancy.Organization, id uuid.UUID,
		reason string, spend Spend) error
	// TriggerEpisode reads what an episode contributes to the investigation it starts.
	TriggerEpisode(ctx context.Context, org tenancy.Organization,
		episode uuid.UUID) (Trigger, error)
	// OpenTriggers reports the organization's open episodes, for inferring a question's
	// subject. Bounded: subject inference reads recent context, not history.
	OpenTriggers(ctx context.Context, org tenancy.Organization, limit int) ([]Trigger, error)
	// InvestigationCandidates reports the enabled integrations whose types the router may
	// select among, with their sealed credentials for the runs.
	InvestigationCandidates(ctx context.Context, org tenancy.Organization,
	) ([]integrations.Integration, error)
	// RecordCredentialUnseal writes the audit event for one credential unseal, before
	// the credential is used; a use that cannot be recorded does not happen.
	RecordCredentialUnseal(ctx context.Context, org tenancy.Organization, id uuid.UUID,
		purpose string) error
	// WorkloadInventory reads a bounded digest of the change ledger's current workload
	// identities — a navigation index for the autonomous orientation, never evidence.
	WorkloadInventory(ctx context.Context, org tenancy.Organization,
		limit int) ([]string, error)
}

// Brief is everything the reasoner may consult for one decision. The text within it —
// subject, question, tool results — originates in a customer's systems and stays
// untrusted: it may steer which declared tool is called next, and nothing else.
type Brief struct {
	Subject     string
	Question    string
	WindowFrom  time.Time
	WindowUntil time.Time
	// Sources are the router's selection: each integration and the tools its type offers.
	Sources []BriefSource
	// Runs is every execution so far, in ordinal order, contents included.
	Runs []ToolRun
	// SpendSoFar is what the investigation's reasoning has consumed before this
	// decision. The reasoning layer refuses a non-concluding decision past the spend
	// ceiling with it — the backstop that makes a runaway structurally impossible even
	// if a loop forgets its own ceiling check.
	SpendSoFar Spend
	// MustConclude says reads are over: the round budget is spent, and this decision must
	// carry findings.
	MustConclude bool
}

// BriefSource is one selected integration and what may be read from it.
type BriefSource struct {
	Integration integrations.Integration
	Tools       []integrations.Tool
}

// Decision is what the reasoner returned: further reads, or the conclusion.
type Decision struct {
	// Calls are the reads to perform next; empty means conclude.
	Calls []ToolCall
	// Findings is the conclusion, possibly empty when nothing was established.
	Findings []Finding
	// Spend is what producing this decision cost.
	Spend Spend
}

// ToolCall is one proposed read.
type ToolCall struct {
	Tool      string
	Arguments map[string]any
}

// Reasoner is the model boundary, in this domain's vocabulary. Implemented by the
// reasoning infrastructure; a test stands a fake here, which is the surviving test seam
// of the recorded-transcript mechanism.
type Reasoner interface {
	Decide(ctx context.Context, brief Brief) (Decision, error)
}
