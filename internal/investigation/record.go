// Package investigation owns the investigation domain: opening one from an alert incident
// or an operator's question, offering every grant-supported connected source to an
// autonomous Exchange, running bounded read tools against them, and persisting
// OPERATIONAL PROVENANCE — what was triggered, which integrations were offered, which
// tools ran over which window, what each returned or failed with, and the findings with
// their sources.
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

	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// Status is where an investigation has got to. Persisted as the integer in the column;
// the values are frozen.
type Status int16

const (
	StatusRunning Status = iota + 1
	StatusConcluded
	StatusFailed
	StatusCancelled
)

// The ceilings that can force a concluding turn, as stopped_by records them. Persisted
// text, frozen like an enum: the operator view and its clients key on these words.
// Empty means the model concluded freely.
const (
	StoppedBySpend         = "spend"
	StoppedByToolRuns      = "tool_runs"
	StoppedByReasonerTurns = "reasoner_turns"
	StoppedByWallClock     = "wall_clock"
	StoppedByStagnation    = "stagnation"
	// StoppedByContext is the turn whose transcript alone would outgrow the model's
	// working budget. It joins the others rather than becoming a failure for the same
	// reason they are not failures: "we stopped reading" is a true thing to say about a
	// partial answer, and "we found nothing" is not.
	StoppedByContext = "context"
)

func (s Status) String() string {
	switch s {
	case StatusRunning:
		return "running"
	case StatusConcluded:
		return "concluded"
	case StatusFailed:
		return "failed"
	case StatusCancelled:
		return "cancelled"
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

var (
	ErrUnknown             = errors.New("investigation unknown")
	ErrAlreadyEnded        = errors.New("investigation has already ended")
	ErrIncidentUnknown     = errors.New("incident unknown")
	ErrReasonerUnavailable = errors.New("the reasoning boundary is unavailable")
	ErrBadCursor           = errors.New("after is not a page position from a previous response")
)

// Finding is one thing the investigation established, tied to the runs that support it.
type Finding struct {
	ID        string `json:"id"`
	Statement string `json:"statement"`
	// Kind is the finding's causal role — FindingCause and its siblings. Empty
	// on a finding concluded before the vocabulary existed; required from the
	// autonomous conclusion, enforced where it is decoded.
	Kind string `json:"kind"`
	// Confidence is categorical — confirmed, likely, possible — never an invented
	// numeric certainty. Empty exactly when Kind is.
	Confidence string `json:"confidence"`
	// Mechanism is required for causal findings and explains how the cause produced impact.
	Mechanism string `json:"mechanism"`
	// Sources are one-based ordinals among the investigation's recorded tool runs. Every
	// finding cites at least one — enforced when the reasoner's answer is decoded — so a
	// statement nothing was read for cannot be stored as established.
	Sources []int `json:"runRefs"`
}

const (
	FindingCause              = "cause"
	FindingContributingFactor = "contributing_factor"
	FindingSymptom            = "symptom"
	FindingTrigger            = "trigger"
	FindingPropagation        = "propagation"
	FindingRuledOut           = "ruled_out"
	FindingUnresolved         = "unresolved"
	// FindingObservation is an established fact with no causal role — "the deployed
	// revision is v2.14.1". Every other kind answers "what caused this"; a peacetime
	// question has no cause to name, and without this the answer to one has to claim to
	// be a symptom or a probable cause of an incident that is not happening.
	FindingObservation = "observation"
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
		FindingCause,
		FindingTrigger,
		FindingContributingFactor,
		FindingSymptom,
		FindingPropagation,
		FindingRuledOut,
		FindingUnresolved,
		FindingObservation,
	}
	Confidences = []string{
		ConfidenceConfirmed,
		ConfidenceLikely,
		ConfidencePossible,
	}
)

// Spend is what the reasoning behind an investigation consumed.
type Spend struct {
	InputTokens  int64
	OutputTokens int64
	MicroCents   int64
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
	ID             uuid.UUID
	OrgID          string
	IncidentID     uuid.UUID
	IntegrationID  uuid.UUID
	Question       string
	ConversationID uuid.UUID
	Turn           int
	Subject        string
	WindowFrom     time.Time
	WindowUntil    time.Time
	Status         Status
	Executing      bool
	Conclusion     Conclusion
	StoppedBy      string
	Error          string
	Spend          Spend
	CreatedBy      string
	CreatedAt      time.Time
	ConcludedAt    time.Time
}

// Source is one integration the investigation was offered, with the reason recorded.
type Source struct {
	IntegrationID uuid.UUID
	Rank          int
	Reason        string
	SelectedAt    time.Time
}

type ToolRun struct {
	IntegrationID uuid.UUID
	Ordinal       int
	Tool          string
	Purpose       string
	HypothesisID  string
	Arguments     map[string]any
	WindowFrom    time.Time
	WindowUntil   time.Time
	WindowApplied bool
	Outcome       RunOutcome
	Truncated     bool
	Summary       string
	Sources       []string
	StartedAt     time.Time
	FinishedAt    time.Time
	// Error is why a failed run failed.
	Error string
	// Content is the run's full answer, held for the reasoner
	//within the running investigation and never persisted
	Content any
}

// NewInvestigation is what a create records.
type NewInvestigation struct {
	IncidentID    uuid.UUID
	IntegrationID uuid.UUID
	Question      string
	Subject       string
	WindowFrom    time.Time
	WindowUntil   time.Time
	CreatedBy     string
}

// Trigger is what an incident contributes when it starts an investigation.
type Trigger struct {
	IncidentID    uuid.UUID
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

// Query is what the investigations listing accepts: a position, and what to narrow by.
type Query struct {
	Page       Page
	IncidentID uuid.UUID
}

// List is a page of an organization's investigations, newest first.
type List struct {
	Investigations []Investigation
	Next           string
}

type OfferedSource struct {
	Integration integrations.Integration
	Tools       []integrations.Tool
}

type ToolCall struct {
	Tool      string
	Arguments map[string]any
}

// Store is everything the Investigation domain needs from durable state.
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
	// QueryInvestigations reports a page, newest first, narrowed by the query.
	QueryInvestigations(ctx context.Context, who authz.Principal, org tenancy.Organization,
		query Query) (List, error)
	// RecordSource writes one offered source.
	RecordSource(ctx context.Context, org tenancy.Organization, id uuid.UUID,
		source Source) error
	// RecordToolRun writes one execution as it finished.
	RecordToolRun(ctx context.Context, org tenancy.Organization, id uuid.UUID,
		run ToolRun) error
	// ConcludeInvestigation ends one with its concluding document and spend. stoppedBy
	// names the ceiling that forced the concluding turn, empty when the model concluded
	// freely. The document travels whole because its three parts are written together
	// and mean nothing apart.
	ConcludeInvestigation(ctx context.Context, org tenancy.Organization, id uuid.UUID,
		conclusion Conclusion, stoppedBy string, spend Spend) error
	// FailInvestigation ends one with the reason it could not conclude.
	FailInvestigation(ctx context.Context, org tenancy.Organization, id uuid.UUID,
		reason string, spend Spend) error
	// TriggerIncident reads what an incident contributes to the investigation it starts.
	TriggerIncident(ctx context.Context, org tenancy.Organization,
		incident uuid.UUID) (Trigger, error)
	// OpenTriggers reports the organization's open incidents, for inferring a question's
	// subject. Bounded: subject inference reads recent context, not history.
	OpenTriggers(ctx context.Context, org tenancy.Organization, limit int) ([]Trigger, error)
	// InvestigationCandidates reports the enabled integrations an investigation may be
	// offered, with their sealed credentials for the runs.
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
	// ConversationBrief reads what a conversation contributes to its next turn: the
	// bounded verbatim tail of what was said and prior turns' cited findings.
	// Never copied tool payloads — a finding
	// already names the runs that established it, and those runs are still in the record.
	ConversationBrief(ctx context.Context, org tenancy.Organization,
		conversation uuid.UUID, tail int) (Brief, error)
	// DrainConversation opens the next turn of a conversation from whatever messages
	// arrived while this one was running, and reports whether one opened. It is called at
	// the terminal boundary — the "next safe point" — because that is the moment a second
	// agent can start without two of them writing one context.
	//
	// Nothing queued means nothing opens, which is the ordinary end of every turn nobody
	// interrupted. lead is how far back the new turn's window reaches.
	DrainConversation(ctx context.Context, org tenancy.Organization,
		conversation uuid.UUID, lead time.Duration) (bool, error)
}
