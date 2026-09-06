package investigation

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Brief is bounded durable context carried between Conversation turns. It references
// prior Tool Runs instead of copying their payloads.

// The brief's own bounds. Every one is here because a conversation that has run for hours
// must not assemble an orientation that grows without limit.
const (
	// BriefRecentMessages is the bounded verbatim message tail.
	BriefRecentMessages = 12
	// BriefMaxFindings bounds how many prior cited findings a brief carries.
	BriefMaxFindings = 40
	// BriefMaxConstraints bounds remembered recommendations and failed reads.
	BriefMaxConstraints = 12
	// BriefMaxOperatorStatements bounds the deterministic, history-wide sample of older
	// person-authored testimony retained outside the recent verbatim tail.
	BriefMaxOperatorStatements = 12
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
	Actor           string
	Text            string
	Sequence        int64
	CreatedAt       time.Time
	InvestigationID uuid.UUID
}

// PriorFinding retains the canonical origin of an earlier observation.
type PriorFinding struct {
	InvestigationID uuid.UUID
	Turn            int
	Statement       string
	Kind            string
	Confidence      string
	Runs            []int
	EvidenceRefs    []EvidenceRef
}

func (p PriorFinding) References() []EvidenceRef {
	refs := append([]EvidenceRef{}, p.EvidenceRefs...)
	for _, run := range p.Runs {
		refs = append(refs, EvidenceRef{InvestigationID: p.InvestigationID, ToolRunOrdinal: run})
	}
	return refs
}

// Reference renders the exact pairs accepted by the conclusion's evidence_refs field.
func (p PriorFinding) Reference() string {
	encoded, _ := json.Marshal(p.References())
	return string(encoded)
}

// Brief is one conversation's contribution to a turn's Orientation.
type Brief struct {
	MissingEvidence bool
	ConversationID  string
	Subject         string
	// Turn is this turn's one-based position, so the agent knows it is not the first.
	Turn int
	// Recent is the verbatim tail, oldest first.
	Recent []BriefMessage
	// RecentFrom is the sequence the verbatim tail starts at.
	RecentFrom int64
	// OperatorStatements are bounded older person-authored messages retained as
	// untrusted testimony, so durable operator facts do not disappear with a long tail.
	OperatorStatements []BriefMessage
	// Findings are prior turns' cited findings.
	Findings []PriorFinding
	// Limitations are gaps and unresolved constraints declared by prior conclusions.
	Limitations []string
	// FailedReads are the prior turns' reads that did not work.
	FailedReads []string
	// Recommended is what prior turns already advised.
	Recommended []string
	// Identifiers are what the prior turns actually read — channel ids, repository ids —
	Identifiers []string
}
