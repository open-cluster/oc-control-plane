package execution

import (
	"time"

	"github.com/google/uuid"
)

// Snapshot is the public projection of an authorized Chat or Investigation execution.
type Snapshot struct {
	ExecutionID     uuid.UUID      `json:"executionId,omitzero"`
	ConversationID  uuid.UUID      `json:"conversationId,omitzero"`
	InvestigationID uuid.UUID      `json:"investigationId,omitzero"`
	Status          string         `json:"status"`
	DraftText       string         `json:"draftText,omitempty"`
	Message         *Message       `json:"message,omitempty"`
	ConclusionURL   string         `json:"conclusionUrl,omitempty"`
	Tools           []ToolActivity `json:"tools"`
}

type Message struct {
	Sequence int64  `json:"sequence"`
	Text     string `json:"text"`
}

type ToolActivity struct {
	ToolRunID  uuid.UUID  `json:"toolRunId"`
	Tool       string     `json:"tool"`
	Status     string     `json:"status"`
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	Truncated  bool       `json:"truncated"`
}

func (snapshot Snapshot) Terminal() bool {
	switch snapshot.Status {
	case "completed", "failed", "cancelled":
		return true
	default:
		return false
	}
}
