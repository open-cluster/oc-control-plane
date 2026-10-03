package investigation

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

const (
	BriefRecentMessages = 12
	BriefMaxFindings    = 40
	BriefMaxConstraints = 12
	BriefMessageBound   = 1024
)

type BriefMessage struct {
	Answer          *Conclusion `json:"answer,omitempty"`
	FromPerson      bool        `json:"fromPerson"`
	Actor           string      `json:"actor,omitempty"`
	Text            string      `json:"text"`
	Sequence        int64       `json:"sequence,omitempty"`
	CreatedAt       time.Time   `json:"createdAt,omitzero"`
	InvestigationID uuid.UUID   `json:"investigationId,omitzero"`
}

type PriorFinding struct {
	InvestigationID uuid.UUID
	Turn            int
	Statement       string
	Kind            string
	Confidence      string
	Runs            []int
	EvidenceRefs    []EvidenceRef
	ObservedAt      time.Time
	WindowFrom      time.Time
	WindowUntil     time.Time
}

func (p PriorFinding) References() []EvidenceRef {
	refs := append([]EvidenceRef{}, p.EvidenceRefs...)
	for _, run := range p.Runs {
		refs = append(refs, EvidenceRef{InvestigationID: p.InvestigationID, ToolRunOrdinal: run})
	}
	return refs
}

func (p PriorFinding) Reference() string {
	encoded, _ := json.Marshal(p.References())
	return string(encoded)
}

type Brief struct {
	MissingEvidence bool
	Turn            int
	Recent          []BriefMessage
	Findings        []PriorFinding
	Limitations     []string
}
