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

type AssignedMessage struct {
	Sequence  int64     `json:"sequence"`
	Actor     string    `json:"actor"`
	CreatedAt time.Time `json:"createdAt"`
	Text      string    `json:"text"`
}

type ConversationOrigin struct {
	IntegrationID uuid.UUID
	Channel       string
	Thread        string
}

type EvidenceRef struct {
	InvestigationID uuid.UUID `json:"investigationId"`
	ToolRunOrdinal  int       `json:"toolRunOrdinal"`
}

const MissingEvidenceStatement = "Some cited Tool Runs are no longer available. Retained Findings have not been reverified."

func (c *Conclusion) MarkMissingEvidence() {
	for _, limitation := range c.Limitations {
		if limitation.Type == LimitationMissingTelemetry && limitation.Statement == MissingEvidenceStatement {
			return
		}
	}
	c.Limitations = append(c.Limitations, Limitation{
		Type: LimitationMissingTelemetry, Statement: MissingEvidenceStatement, RunRefs: []int{},
	})
}

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
	Runs            []int
	EvidenceRefs    []EvidenceRef
	ObservedAt      time.Time
	WindowFrom      time.Time
	WindowUntil     time.Time
}

func (p PriorFinding) References() []EvidenceRef {
	refs := append([]EvidenceRef{}, p.EvidenceRefs...)
	for _, run := range p.Runs {
		refs = append(refs, EvidenceRef{
			InvestigationID: p.InvestigationID, ToolRunOrdinal: run,
		})
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
	OpenHypotheses  []string
	Limitations     []string
}

type HistoryPage struct {
	NextBefore      int64          `json:"nextBefore"`
	MissingEvidence bool           `json:"missingEvidence,omitempty"`
	Limitations     []string       `json:"limitations,omitempty"`
	Exchange        []BriefMessage `json:"exchange"`
	Truncated       bool           `json:"truncated,omitempty"`
}
