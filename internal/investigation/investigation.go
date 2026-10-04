package investigation

import (
	"context"
	"time"

	"github.com/google/uuid"
)

type Agent interface {
	Run(context.Context, uuid.UUID, Investigation) error
}

const MaxHypothesisSnapshotItems = 8

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
	Statement    string        `json:"statement"`
	Kind         FindingKind   `json:"kind"`
	Mechanism    string        `json:"mechanism"`
	RunRefs      []int         `json:"runRefs"`
	EvidenceRefs []EvidenceRef `json:"evidenceRefs,omitempty"`
}

type Conclusion struct {
	Status      ConclusionStatus   `json:"status"`
	Summary     string             `json:"summary"`
	Impact      Impact             `json:"impact"`
	Findings    []Finding          `json:"findings"`
	Hypotheses  []HypothesisResult `json:"hypotheses"`
	Actions     []ActionProposal   `json:"actions"`
	Limitations []Limitation       `json:"limitations"`
}

type ConclusionStatus string

const (
	VerifiedCause        ConclusionStatus = "verified_cause"
	SupportedExplanation ConclusionStatus = "supported_explanation"
	Inconclusive         ConclusionStatus = "inconclusive"
	AnswerOnly           ConclusionStatus = "answer_only"
)

var ConclusionStatuses = []string{
	string(VerifiedCause),
	string(SupportedExplanation),
	string(Inconclusive),
	string(AnswerOnly),
}

type Impact struct {
	Summary string `json:"summary"`
	RunRefs []int  `json:"runRefs"`
}

type FindingKind string

const (
	FindingCause              FindingKind = "cause"
	FindingContributingFactor FindingKind = "contributing_factor"
	FindingRuledOut           FindingKind = "ruled_out"
	FindingObservation        FindingKind = "observation"
)

var FindingKinds = []string{
	string(FindingCause),
	string(FindingContributingFactor),
	string(FindingObservation),
	string(FindingRuledOut),
}

type HypothesisStatus string

const (
	HypothesisExploring  HypothesisStatus = "exploring"
	HypothesisSupported  HypothesisStatus = "supported"
	HypothesisRuledOut   HypothesisStatus = "ruled_out"
	HypothesisUnresolved HypothesisStatus = "unresolved"
)

var HypothesisStatuses = []string{
	string(HypothesisExploring),
	string(HypothesisSupported),
	string(HypothesisRuledOut),
	string(HypothesisUnresolved),
}

type HypothesisResult struct {
	ID        string           `json:"id"`
	Statement string           `json:"statement"`
	Status    HypothesisStatus `json:"status"`
	Test      string           `json:"test"`
	RunRefs   []int            `json:"runRefs"`
}

type ActionProposal struct {
	Title        string `json:"title"`
	Rationale    string `json:"rationale"`
	Verification string `json:"verification"`
	RunRefs      []int  `json:"runRefs"`
}

type LimitationType string

const (
	LimitationMissingTelemetry     LimitationType = "missing_telemetry"
	LimitationMissingAccess        LimitationType = "missing_access"
	LimitationContradiction        LimitationType = "contradiction"
	LimitationUnresolvedAssumption LimitationType = "unresolved_assumption"
	LimitationEssentialHumanInput  LimitationType = "essential_human_input"
)

var LimitationTypes = []string{
	string(LimitationMissingTelemetry),
	string(LimitationMissingAccess),
	string(LimitationContradiction),
	string(LimitationUnresolvedAssumption),
	string(LimitationEssentialHumanInput),
}

type Limitation struct {
	Type             LimitationType `json:"type"`
	Statement        string         `json:"statement"`
	RunRefs          []int          `json:"runRefs"`
	MessageSequences []int64        `json:"messageSequences,omitempty"`
}

const (
	MaxConclusionActions = 8
	MaxActionTextLength  = 512
	MaxSummaryLength     = 4096
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
