package investigation

import (
	"time"

	"github.com/google/uuid"
)

type Status int16

const (
	StatusRunning Status = iota + 1
	StatusConcluded
	StatusFailed
	StatusCancelled
)

const (
	StoppedByToolRuns      = "tool_runs"
	StoppedByReasonerTurns = "reasoner_turns"
	StoppedByWallClock     = "wall_clock"
	StoppedByStagnation    = "stagnation"
	StoppedByContext       = "context"
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

type RunOutcome int16

const (
	RunSucceeded RunOutcome = iota + 1
	RunFailed
)

type Finding struct {
	ID           string        `json:"id"`
	Statement    string        `json:"statement"`
	Kind         string        `json:"kind"`
	Confidence   string        `json:"confidence"`
	Mechanism    string        `json:"mechanism"`
	Sources      []int         `json:"runRefs"`
	EvidenceRefs []EvidenceRef `json:"evidenceRefs,omitempty"`
}

const (
	FindingCause              = "cause"
	FindingContributingFactor = "contributing_factor"
	FindingSymptom            = "symptom"
	FindingTrigger            = "trigger"
	FindingPropagation        = "propagation"
	FindingRuledOut           = "ruled_out"
	FindingUnresolved         = "unresolved"
	FindingObservation        = "observation"
)

const (
	ConfidenceConfirmed = "confirmed"
	ConfidenceLikely    = "likely"
	ConfidencePossible  = "possible"
)

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

type Usage struct {
	InputTokens  int64
	OutputTokens int64
}

func (u Usage) Add(other Usage) Usage {
	return Usage{
		InputTokens:  u.InputTokens + other.InputTokens,
		OutputTokens: u.OutputTokens + other.OutputTokens,
	}
}

type Investigation struct {
	ID             uuid.UUID
	OrgID          string
	IncidentID     uuid.UUID
	Question       string
	ConversationID uuid.UUID
	Turn           int
	Subject        string
	WindowFrom     time.Time
	WindowUntil    time.Time
	Status         Status
	Executing      bool
	ClaimToken     uuid.UUID `json:"-"`
	Conclusion     Conclusion
	StoppedBy      string
	Error          string
	Usage          Usage
	CreatedBy      string
	CreatedAt      time.Time
	ConcludedAt    time.Time
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
	Error         string
	Content       any
}

type NewInvestigation struct {
	IncidentID  uuid.UUID
	Question    string
	Subject     string
	WindowFrom  time.Time
	WindowUntil time.Time
	CreatedBy   string
}

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

type Page struct {
	Limit int
	After string
}

type Query struct {
	Page       Page
	IncidentID uuid.UUID
}

type List struct {
	Investigations []Investigation
	Next           string
}

type ToolCall struct {
	Tool      string
	Arguments map[string]any
}
