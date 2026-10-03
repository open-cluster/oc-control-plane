package investigation

import (
	"context"
	"github.com/google/uuid"
)

type Agent interface {
	Run(context.Context, uuid.UUID, Investigation) error
}

const MaxHypothesisSnapshotItems = 8

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
	string(VerifiedCause), string(SupportedExplanation), string(Inconclusive), string(AnswerOnly),
}

type Impact struct {
	Summary string `json:"summary"`
	RunRefs []int  `json:"runRefs"`
}

type HypothesisStatus string

const (
	HypothesisExploring  HypothesisStatus = "exploring"
	HypothesisSupported  HypothesisStatus = "supported"
	HypothesisRuledOut   HypothesisStatus = "ruled_out"
	HypothesisUnresolved HypothesisStatus = "unresolved"
)

var HypothesisStatuses = []string{
	string(HypothesisExploring), string(HypothesisSupported),
	string(HypothesisRuledOut), string(HypothesisUnresolved),
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
	string(LimitationMissingTelemetry), string(LimitationMissingAccess),
	string(LimitationContradiction), string(LimitationUnresolvedAssumption),
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
