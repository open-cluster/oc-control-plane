package investigation

import (
	"context"

	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
)

// Agent owns one Investigation's reasoning, Tool execution, provenance, and terminal state.
type Agent interface {
	Run(context.Context, tenancy.Organization, Investigation) error
}

const (
	HypothesisSnapshotVersion  = 1
	MaxHypothesisSnapshotItems = 8
)

// Conclusion is the versioned, operator-facing result of an investigation.
type Conclusion struct {
	Status      ConclusionStatus   `json:"status"`
	Summary     string             `json:"summary"`
	Impact      ImpactAssessment   `json:"impact"`
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

type ImpactStatus string

const (
	ImpactKnown   ImpactStatus = "known"
	ImpactPartial ImpactStatus = "partial"
	ImpactUnknown ImpactStatus = "unknown"
)

var ImpactStatuses = []string{string(ImpactKnown), string(ImpactPartial), string(ImpactUnknown)}

type ImpactAssessment struct {
	Status           ImpactStatus `json:"status"`
	CurrentState     string       `json:"currentState"`
	AffectedServices []string     `json:"affectedServices"`
	AffectedUsers    []string     `json:"affectedUsers"`
	Summary          string       `json:"summary"`
	RunRefs          []int        `json:"runRefs"`
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

type ActionType string

const (
	ActionMitigate ActionType = "mitigate"
	ActionRollback ActionType = "rollback"
	ActionVerify   ActionType = "verify"
	ActionFix      ActionType = "fix"
	ActionMonitor  ActionType = "monitor"
)

var ActionTypes = []string{
	string(ActionMitigate), string(ActionRollback), string(ActionVerify),
	string(ActionFix), string(ActionMonitor),
}

type ActionRisk string

const (
	RiskLow    ActionRisk = "low"
	RiskMedium ActionRisk = "medium"
	RiskHigh   ActionRisk = "high"
)

var ActionRisks = []string{string(RiskLow), string(RiskMedium), string(RiskHigh)}

type ActionProposal struct {
	Title            string     `json:"title"`
	Type             ActionType `json:"type"`
	Rationale        string     `json:"rationale"`
	Risk             ActionRisk `json:"risk"`
	Reversible       bool       `json:"reversible"`
	RequiresApproval bool       `json:"requiresApproval"`
	Verification     string     `json:"verification"`
	RunRefs          []int      `json:"runRefs"`
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
	Type      LimitationType `json:"type"`
	Statement string         `json:"statement"`
	RunRefs   []int          `json:"runRefs"`
}

// The concluding document's record bounds, enforced where it is decoded and again
// before the record is written — the same twice-enforced pattern the citation invariant
// uses, with one set of numbers for both.
const (
	MaxConclusionActions = 8
	MaxActionTextLength  = 512
	// MaxAnswerLength bounds the direct reply. Long enough for a paragraph that names
	// identifiers and versions; past this it is a report, and the report is the findings.
	MaxSummaryLength = 4096
)
