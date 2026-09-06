package investigation

import "github.com/google/uuid"

// EvidenceRef identifies a Tool Run outside the containing Investigation.
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
	c.Limitations = append(c.Limitations, Limitation{Type: LimitationMissingTelemetry, Statement: MissingEvidenceStatement, RunRefs: []int{}})
}
