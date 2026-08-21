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
	// Trigger is the alert episode's own metadata; nil when an operator question opened
	// the investigation without one, or the trigger could not be read.
	Trigger *Trigger
	// Sources is every offered integration with the tools its verified grants support,
	// in stable name order. The whole universe, not a routed subset: nothing may
	// permanently prevent the investigator from reaching a connected source.
	Sources []OfferedSource
	// Inventory is the change ledger's bounded workload digest — navigation, never
	// evidence. Empty when the ledger holds nothing.
	Inventory []string
}

// AgentCall is one read the Exchange proposed. The identifier is the provider's
// own for this call, echoed back with the result so the transcript pairs them.
type AgentCall struct {
	ID        string
	Tool      string
	Arguments map[string]any
}

// CallResult feeds one call's outcome back into the Exchange: the run exactly as
// provenance recorded it, content included, paired to the call that asked for it.
type CallResult struct {
	CallID string
	Run    ToolRun
}

// Conclusion is the concluding move's document — the direct answer, findings with kind
// and confidence, plus the recommended next steps. Never itself a persisted record:
// what is stored is the Investigation's answer, findings and next steps, checked on the
// way in.
type Conclusion struct {
	// Answer is the reply in the operator's own words. A question expects one; an
	// episode-triggered investigation may carry none, and its findings are the answer.
	Answer    string
	Findings  []Finding
	NextSteps []string
}

// The concluding document's record bounds, enforced where it is decoded and again
// before the record is written — the same twice-enforced pattern the citation invariant
// uses, with one set of numbers for both.
const (
	MaxConclusionNextSteps = 8
	MaxNextStepLength      = 512
	// MaxAnswerLength bounds the direct reply. Long enough for a paragraph that names
	// identifiers and versions; past this it is a report, and the report is the findings.
	MaxAnswerLength = 4096
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
