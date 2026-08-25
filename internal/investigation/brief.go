package investigation

import (
	"strconv"
	"strings"
)

// THE CONVERSATION BRIEF — what a follow-up turn knows that a first turn does not.
//
// A turn opens its own Exchange from nothing; the Conversation is what carries continuity
// between them. The brief is that continuity, and it is assembled ONLY from what the
// platform already holds: a verbatim tail of what was actually said and the prior turns'
// findings with their citations expressed as REFERENCES — the turn and
// the run ordinal — never as copied tool payloads.
//
// Copying payloads would be the obvious thing and the wrong one. A finding already carries
// the ordinals of the runs that established it, and those runs are still in the record; a
// second copy of what they returned would double every long conversation's context to say
// something the citation already says.

// The brief's own bounds. Every one is here because a conversation that has run for hours
// must not assemble an orientation that grows without limit.
const (
	// BriefRecentMessages is the bounded verbatim message tail.
	BriefRecentMessages = 12
	// BriefMaxFindings bounds how many prior cited findings a brief carries.
	BriefMaxFindings = 40
	// BriefMaxConstraints bounds remembered recommendations and failed reads.
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
		return "turn " + strconv.Itoa(p.Turn)
	}
	ordinals := make([]string, 0, len(p.Runs))
	for _, run := range p.Runs {
		ordinals = append(ordinals, strconv.Itoa(run))
	}
	return "turn " + strconv.Itoa(p.Turn) + " run " + strings.Join(ordinals, ", ")
}

// Brief is one conversation's contribution to a turn's Orientation.
type Brief struct {
	ConversationID string
	Subject        string
	// OriginIntegrationID, OriginChannel, and OriginThread identify the provider
	// thread that originated this Conversation. Browser Conversations leave them empty.
	OriginIntegrationID string
	OriginChannel       string
	OriginThread        string
	// Turn is this turn's one-based position, so the agent knows it is not the first.
	Turn int
	// Recent is the verbatim tail, oldest first.
	Recent []BriefMessage
	// RecentFrom is the sequence the verbatim tail starts at.
	RecentFrom int64
	// Findings are prior turns' cited findings.
	Findings []PriorFinding
	// FailedReads are the prior turns' reads that did not work.
	FailedReads []string
	// Recommended is what prior turns already advised.
	Recommended []string
	// Identifiers are what the prior turns actually read — channel ids, repository ids —
	Identifiers []string
}
