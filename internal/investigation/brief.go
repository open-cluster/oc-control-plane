package investigation

import (
	"strings"
)

// THE CONVERSATION BRIEF — what a follow-up turn knows that a first turn does not.
//
// A turn opens its own Exchange from nothing; the Conversation is what carries continuity
// between them. The brief is that continuity, and it is assembled ONLY from what the
// platform already holds: the running summary, a verbatim tail of what was actually said,
// and the prior turns' findings with their citations expressed as REFERENCES — the turn and
// the run ordinal — never as copied tool payloads.
//
// Copying payloads would be the obvious thing and the wrong one. A finding already carries
// the ordinals of the runs that established it, and those runs are still in the record; a
// second copy of what they returned would double every long conversation's context to say
// something the citation already says.

// The brief's own bounds. Every one is here because a conversation that has run for hours
// must not assemble an orientation that grows without limit.
const (
	// BriefRecentMessages is how many messages stay verbatim behind the summary. Enough
	// for the last exchange or two to read as it was written.
	BriefRecentMessages = 12
	// BriefMaxFindings bounds how many prior findings a brief carries, newest turns
	// first. A conversation with a hundred established facts is one whose summary is
	// doing the work.
	BriefMaxFindings = 40
	// BriefMaxConstraints bounds the durable instructions a summary keeps.
	BriefMaxConstraints = 12
	// BriefMaxIdentifiers bounds the service and resource identifiers in play.
	BriefMaxIdentifiers = 30
	// BriefMessageBound bounds one remembered message.
	BriefMessageBound = 1024
)

// BriefMessage is one thing said, as a turn is told about it. It carries who said it
// because a shared conversation that cannot say who asked what is not a record — and
// because the model must be able to tell an operator's instruction from its own earlier
// answer.
type BriefMessage struct {
	// FromPerson distinguishes what somebody said from what the agent answered.
	FromPerson bool
	// Actor is who said it, for attribution. Never a credential and never an email
	// address the model has any use for; a display name.
	Actor string
	Text  string
}

// PriorFinding is something an earlier turn established, with its citation as a reference.
// Turn and Runs together name exactly where the evidence is without carrying any of it.
type PriorFinding struct {
	Turn       int
	Statement  string
	Kind       string
	Confidence string
	Runs       []int
}

// Reference renders the citation an operator or a model can follow: which turn, which runs.
func (p PriorFinding) Reference() string {
	if len(p.Runs) == 0 {
		return "turn " + itoa(p.Turn)
	}
	ordinals := make([]string, 0, len(p.Runs))
	for _, run := range p.Runs {
		ordinals = append(ordinals, itoa(run))
	}
	return "turn " + itoa(p.Turn) + " run " + strings.Join(ordinals, ", ")
}

// Summary is the structured running summary older turns compact into.
//
// The SECTIONS are the contract. A free-text blob would be a summary nobody could check;
// these can be compared field by field against what the conversation actually holds, which
// is what makes the compaction test deterministic rather than a judge's opinion.
type Summary struct {
	Version int
	// CoversThrough is the message sequence this summary accounts for. Everything after
	// it is still carried verbatim.
	CoversThrough int64
	// Goal is what the conversation is for, in the words it was opened with.
	Goal string
	// Problem is the incident or question the conversation started from.
	Problem string
	// Constraints are the operator's own durable instructions — "ignore the database,
	// look at deployments" — kept VERBATIM. A paraphrase of an instruction is an
	// instruction somebody did not give.
	Constraints []string
	// Established, RuledOut and Open are prior findings sorted by what they are for. A
	// hypothesis that was ruled out stays ruled out unless new evidence appears, and it
	// only stays ruled out if the next turn is told.
	Established []PriorFinding
	RuledOut    []PriorFinding
	Open        []PriorFinding
	// FailedReads are the reads that did not work, so a gap in an answer stays explained
	// rather than becoming silent after compaction.
	FailedReads []string
	// Identifiers are the services and resources in play — the names the conversation has
	// been about.
	Identifiers []string
}

// Empty reports whether this summary would tell a turn nothing.
func (s Summary) Empty() bool {
	return s.Goal == "" && s.Problem == "" && len(s.Constraints) == 0 &&
		len(s.Established) == 0 && len(s.RuledOut) == 0 && len(s.Open) == 0 &&
		len(s.FailedReads) == 0 && len(s.Identifiers) == 0
}

// Brief is one conversation's contribution to a turn's Orientation.
type Brief struct {
	ConversationID string
	Subject        string
	// Turn is this turn's one-based position, so the agent knows it is not the first.
	Turn int
	// Summary is the newest running summary, empty when nothing has been compacted yet.
	Summary Summary
	// Recent is the verbatim tail, oldest first.
	Recent []BriefMessage
	// RecentFrom is the sequence the verbatim tail starts at, so a compaction can record
	// exactly what it accounts for and what is still carried as it was said.
	RecentFrom int64
	// Findings are the prior turns' findings not already inside the summary.
	Findings []PriorFinding
	// FailedReads are the prior turns' reads that did not work, outside the summary.
	FailedReads []string
	// Identifiers are what the prior turns actually read — channel ids, repository ids —
	// outside the summary.
	Identifiers []string
}

// itoa is strconv.Itoa without the import, for the two places this file needs it.
func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var digits [20]byte
	position := len(digits)
	for value > 0 {
		position--
		digits[position] = byte('0' + value%10)
		value /= 10
	}
	if negative {
		position--
		digits[position] = '-'
	}
	return string(digits[position:])
}
