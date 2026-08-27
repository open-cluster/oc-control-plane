package investigation

import (
	"context"
	"time"
)

// THE EXCHANGE BOUNDARY — the investigation's model seam.
//
// The loop holds one Exchange whose provider carries the transcript natively, so the
// seam is exchange-shaped: open once with the orientation, then feed each move's
// results back and receive the next move. It is declared here in the domain's
// vocabulary and implemented by the reasoning infrastructure; this package never
// learns a vendor exists.
//
// It is called an Exchange rather than a conversation because a Conversation is a
// customer-facing record — the multi-turn context a person talks to, holding messages
// and the investigations its turns opened. One word for two things at that distance is
// how a schema ends up with two meanings of the same noun.

// Orientation is everything the investigator is given at open. All of it is
// context the platform already holds — the trigger, the catalog, the ledger — and none
// of it comes from querying a vendor: the investigator itself decides which sources to
// actually read. The text within it originates in a customer's systems and stays
// untrusted for its whole life.
type Orientation struct {
	Subject     string
	Question    string
	WindowFrom  time.Time
	WindowUntil time.Time
	// Trigger is the alert incident's own metadata; nil when an operator question opened
	// the investigation without one, or the trigger could not be read.
	Trigger *Trigger
	// Sources is every offered integration with the tools its verified grants support,
	// in stable name order. The whole universe, not a routed subset: nothing may
	// permanently prevent the investigator from reaching a connected source.
	Sources []OfferedSource
	// Inventory is the change ledger's bounded workload digest — navigation, never
	// evidence. Empty when the ledger holds nothing.
	Inventory []string
	// Preflight contains ordinary numbered reads the platform made only when the trigger
	// supplied exact safe identifiers. They count against the same budget and citation
	// universe as model-requested reads.
	Preflight []ToolRun
	// Brief is what the Conversation this turn belongs to has established so far: the
	// bounded verbatim recent tail and the prior turns' findings with their
	// citations as references. Nil for a single-shot investigation, which has no
	// conversation and therefore nothing to continue from.
	Brief *Brief
}

// AgentCall is one read the Exchange proposed. The identifier is the provider's
// own for this call, echoed back with the result so the transcript pairs them.
type AgentCall struct {
	ID           string
	Tool         string
	Purpose      string
	HypothesisID string
	Arguments    map[string]any
}

// CallResult feeds one call's outcome back into the Exchange: the run exactly as
// provenance recorded it, content included, paired to the call that asked for it.
type CallResult struct {
	CallID   string
	Run      ToolRun
	Semantic bool
}

// UpdateHypothesesToolName is the local semantic tool used to publish the complete
// operator-visible hypothesis snapshot. It never dispatches to a connected source.
const UpdateHypothesesToolName = "update_hypotheses"

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

// Move is one turn's outcome: further reads, or the conclusion. Spend is what
// producing it cost, summed over however many provider calls the turn took.
type Move struct {
	Calls      []AgentCall
	Conclusion *Conclusion
	Spend      Spend
}

// Investigator opens autonomous exchanges. One value serves every investigation; each
// Exchange is its own state.
type Investigator interface {
	OpenExchange(ctx context.Context, orientation Orientation) (Exchange, error)
}

// Exchange is one investigation's running exchange with the model. Not safe for
// concurrent use; one investigation runs its turns in sequence.
type Exchange interface {
	// Next feeds the previous move's results and returns the next move. The first call
	// passes no results. mustConclude withdraws the tools: the returned move must carry
	// the conclusion, and reason says why reads are over — it is rendered to the model,
	// so it is written for the model to act on.
	Next(ctx context.Context, results []CallResult, mustConclude bool,
		reason string) (Move, error)
}
