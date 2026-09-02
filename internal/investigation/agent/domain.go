package agent

import (
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"time"
)

type (
	Investigation    = investigation.Investigation
	OfferedSource    = investigation.OfferedSource
	Source           = investigation.Source
	ToolRun          = investigation.ToolRun
	ToolCall         = investigation.ToolCall
	Spend            = investigation.Spend
	Conclusion       = investigation.Conclusion
	Finding          = investigation.Finding
	ActionProposal   = investigation.ActionProposal
	HypothesisResult = investigation.HypothesisResult
	HypothesisStatus = investigation.HypothesisStatus
	Brief            = investigation.Brief
	Trigger          = investigation.Trigger
	Status           = investigation.Status
)

type orientation struct {
	Subject     string
	Question    string
	WindowFrom  time.Time
	WindowUntil time.Time
	Trigger     *Trigger
	Sources     []OfferedSource
	Inventory   []string
	Preflight   []ToolRun
	Brief       *Brief
}

type toolCall struct {
	ID           string
	Tool         string
	Purpose      string
	HypothesisID string
	Arguments    map[string]any
}

type toolFeedback struct {
	CallID   string
	Run      ToolRun
	Semantic bool
}

type modelMove struct {
	Calls      []toolCall
	Conclusion *Conclusion
	Spend      Spend
}

var HypothesisStatuses = investigation.HypothesisStatuses

const (
	UpdateHypothesesToolName   = "update_hypotheses"
	MaxHypothesisSnapshotItems = investigation.MaxHypothesisSnapshotItems
	HypothesisSnapshotVersion  = investigation.HypothesisSnapshotVersion
	StatusFailed               = investigation.StatusFailed
	StatusConcluded            = investigation.StatusConcluded
	EventStarted               = investigation.EventStarted
	EventProgress              = investigation.EventProgress
	EventToolStarted           = investigation.EventToolStarted
	EventToolCompleted         = investigation.EventToolCompleted
	EventAnswerDelta           = investigation.EventAnswerDelta
	EventConcluded             = investigation.EventConcluded
	EventHypothesesUpdated     = investigation.EventHypothesesUpdated
	EventFailed                = investigation.EventFailed
	RunSucceeded               = investigation.RunSucceeded
	RunFailed                  = investigation.RunFailed
	StoppedBySpend             = investigation.StoppedBySpend
	StoppedByToolRuns          = investigation.StoppedByToolRuns
	StoppedByReasonerTurns     = investigation.StoppedByReasonerTurns
	StoppedByWallClock         = investigation.StoppedByWallClock
	StoppedByStagnation        = investigation.StoppedByStagnation
	StoppedByContext           = investigation.StoppedByContext
	BriefRecentMessages        = investigation.BriefRecentMessages
	MaxActionTextLength        = investigation.MaxActionTextLength
	MaxConclusionActions       = investigation.MaxConclusionActions
	MaxSummaryLength           = investigation.MaxSummaryLength
	FindingSymptom             = investigation.FindingSymptom
	FindingTrigger             = investigation.FindingTrigger
	FindingObservation         = investigation.FindingObservation
	FindingCause               = investigation.FindingCause
	FindingPropagation         = investigation.FindingPropagation
	FindingRuledOut            = investigation.FindingRuledOut
	FindingUnresolved          = investigation.FindingUnresolved
	ConfidencePossible         = investigation.ConfidencePossible
	ConfidenceConfirmed        = investigation.ConfidenceConfirmed
	HypothesisExploring        = investigation.HypothesisExploring
	HypothesisSupported        = investigation.HypothesisSupported
)
