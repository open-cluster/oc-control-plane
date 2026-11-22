package investigation

import (
	"time"

	"github.com/google/uuid"
)

type AssignedMessage struct {
	Sequence  int64     `json:"sequence"`
	Author    string    `json:"author"`
	CreatedAt time.Time `json:"createdAt"`
	Text      string    `json:"text"`
}

type ConversationOrigin struct {
	IntegrationID uuid.UUID
	Channel       string
	Thread        string
}

type EvidenceRef struct {
	ToolRunID       uuid.UUID `json:"toolRunId,omitzero"`
	InvestigationID uuid.UUID `json:"investigationId,omitzero"`
	ToolRunOrdinal  int       `json:"toolRunOrdinal,omitempty"`
}

const MissingEvidenceStatement = "Some cited Tool Runs are no longer available. Retained Findings have not been reverified."

func (c *Conclusion) MarkMissingEvidence() {
	for _, limitation := range c.Limitations {
		if limitation.Type == LimitationMissingTelemetry && limitation.Statement == MissingEvidenceStatement {
			return
		}
	}
	c.Limitations = append(c.Limitations, Limitation{
		Type:      LimitationMissingTelemetry,
		Statement: MissingEvidenceStatement,
	})
}
