package investigation

import (
	"context"
	"time"
)

// THE CONVERSATION BOUNDARY — the autonomous loop's model seam, beside Reasoner.
//
// The deterministic loop re-renders its whole brief every round; the autonomous loop
// holds one conversation whose provider carries the transcript natively, so its seam is
// conversation-shaped: open once with the orientation, then feed each move's results
// back and receive the next move. Like Reasoner, it is declared here in the domain's
// vocabulary and implemented by the reasoning infrastructure; this package never learns
// a vendor exists. Both seams coexist until a scored evaluation picks the winner.

// Orientation is everything the autonomous investigator is given at open. All of it is
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
	Sources []BriefSource
	// Inventory is the change ledger's bounded workload digest — navigation, never
	// evidence. Empty when the ledger holds nothing.
	Inventory []string
}

// AgentCall is one read the conversation proposed. The identifier is the provider's
// own for this call, echoed back with the result so the transcript pairs them.
type AgentCall struct {
	ID        string
	Tool      string
	Arguments map[string]any
}

// CallResult feeds one call's outcome back into the conversation: the run exactly as
// provenance recorded it, content included, paired to the call that asked for it.
type CallResult struct {
	CallID string
	Run    ToolRun
}

// Conclusion is the concluding move's document — findings with kind and confidence,
// plus the recommended next steps. Never itself a persisted record: what is stored is
// the Investigation's findings and next steps, checked on the way in.
type Conclusion struct {
	Findings  []Finding
	NextSteps []string
}

// The recommended next steps' record bounds, enforced where the concluding document is
// decoded and again before the record is written — the same twice-enforced pattern the
// citation invariant uses, with one pair of numbers for both.
const (
	MaxConclusionNextSteps = 8
	MaxNextStepLength      = 512
)

// Move is one turn's outcome: further reads, or the conclusion. Spend is what
// producing it cost, summed over however many provider calls the turn took.
type Move struct {
	Calls      []AgentCall
	Conclusion *Conclusion
	Spend      Spend
}

// Investigator opens autonomous conversations. One value serves every investigation;
// each conversation is its own state.
type Investigator interface {
	OpenConversation(ctx context.Context, orientation Orientation) (Conversation, error)
}

// Conversation is one investigation's running exchange with the model. Not safe for
// concurrent use; one investigation runs its turns in sequence.
type Conversation interface {
	// Next feeds the previous move's results and returns the next move. The first call
	// passes no results. mustConclude withdraws the tools: the returned move must carry
	// the conclusion, and reason says why reads are over — it is rendered to the model,
	// so it is written for the model to act on.
	Next(ctx context.Context, results []CallResult, mustConclude bool,
		reason string) (Move, error)
}
