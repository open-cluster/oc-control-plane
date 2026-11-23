package postmortem

import (
	"encoding/json"
	"errors"
	"maps"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

const NeedsHumanInput = "Needs human input."

type Status string

const (
	StatusDraft    Status = "draft"
	StatusReviewed Status = "reviewed"
)

var (
	ErrUnknown             = errors.New("postmortem unknown")
	ErrNotEligible         = errors.New("incident is not resolved")
	ErrAlreadyExists       = errors.New("postmortem already exists")
	ErrAlreadyReviewed     = errors.New("postmortem is already reviewed")
	ErrUnavailableEvidence = errors.New("postmortem correction cites unavailable evidence")
)

type TimelineEntry struct {
	At              time.Time                   `json:"at"`
	Description     string                      `json:"description"`
	InvestigationID string                      `json:"investigationId,omitempty"`
	EvidenceRefs    []investigation.EvidenceRef `json:"evidenceRefs,omitempty"`
}

type CitedStatement struct {
	InvestigationID string                      `json:"investigationId,omitempty"`
	Statement       string                      `json:"statement"`
	EvidenceRefs    []investigation.EvidenceRef `json:"evidenceRefs,omitempty"`
}

type ActionItem struct {
	Title           string                      `json:"title"`
	Owner           string                      `json:"owner"`
	Deadline        string                      `json:"deadline"`
	Verification    string                      `json:"verification"`
	InvestigationID string                      `json:"investigationId,omitempty"`
	EvidenceRefs    []investigation.EvidenceRef `json:"evidenceRefs,omitempty"`
}

type Postmortem struct {
	IncidentID          uuid.UUID        `json:"incidentId"`
	Status              Status           `json:"status"`
	Revision            int              `json:"revision"`
	Title               string           `json:"title"`
	ExecutiveSummary    string           `json:"executiveSummary"`
	Impact              string           `json:"impact"`
	Detection           string           `json:"detection"`
	Timeline            []TimelineEntry  `json:"timeline"`
	RootCauses          []CitedStatement `json:"rootCauses"`
	ContributingFactors []CitedStatement `json:"contributingFactors"`
	Resolution          string           `json:"resolution"`
	WhatWentWell        []string         `json:"whatWentWell"`
	WhatWentWrong       []string         `json:"whatWentWrong"`
	DetectionGaps       []string         `json:"detectionGaps"`
	ActionItems         []ActionItem     `json:"actionItems"`
	OpenQuestions       []string         `json:"openQuestions"`
	CreatedAt           time.Time        `json:"createdAt"`
	UpdatedAt           time.Time        `json:"updatedAt"`
	ReviewedAt          time.Time        `json:"reviewedAt,omitempty"`
	ReviewedByUserID    *uuid.UUID       `json:"reviewedByUserId,omitempty"`
	historical          investigation.HistoricalCitations
}

func (document Postmortem) MarshalJSON() ([]byte, error) {
	type fields Postmortem
	encoded, err := json.Marshal(fields(document))
	if err != nil {
		return nil, err
	}
	return investigation.WithHistoricalCitations(encoded, document.historical)
}

func (document *Postmortem) UnmarshalJSON(encoded []byte) error {
	type fields Postmortem
	var decoded fields
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return err
	}
	historical, err := investigation.ReadHistoricalCitations(encoded, "timeline", "rootCauses", "contributingFactors", "actionItems")
	if err != nil {
		return err
	}
	*document = Postmortem(decoded)
	document.historical = historical
	return nil
}

type AlertEvent struct {
	Title   string
	Summary string
	At      time.Time
}

type Message struct {
	Author string
	Text   string
	At     time.Time
}

type HumanInput struct {
	Impact        string   `json:"impact"`
	Resolution    string   `json:"resolution"`
	WhatWentWell  []string `json:"whatWentWell"`
	WhatWentWrong []string `json:"whatWentWrong"`
	DetectionGaps []string `json:"detectionGaps"`
}

type GenerationInput struct {
	IncidentID  uuid.UUID
	Title       string
	FirstSeenAt time.Time
	ResolvedAt  time.Time
	AlertEvents []AlertEvent
	Runs        []RunEvidence
	Events      []EventEvidence
	Results     []InvestigationResult
	Messages    []Message
	Human       HumanInput
}

type RunEvidence struct {
	InvestigationID uuid.UUID
	Run             investigation.ToolRun
}

type EventEvidence struct {
	InvestigationID uuid.UUID
	Event           investigation.Event
}

type InvestigationResult struct {
	InvestigationID uuid.UUID
	Conclusion      investigation.Conclusion
}

func ApplyCorrections(document Postmortem, corrections Corrections) Postmortem {
	document.historical = maps.Clone(document.historical)
	if corrections.Title != nil {
		document.Title = *corrections.Title
	}
	if corrections.ExecutiveSummary != nil {
		document.ExecutiveSummary = *corrections.ExecutiveSummary
	}
	if corrections.Impact != nil {
		document.Impact = *corrections.Impact
	}
	if corrections.Detection != nil {
		document.Detection = *corrections.Detection
	}
	if corrections.Timeline != nil {
		document.Timeline = append([]TimelineEntry(nil), (*corrections.Timeline)...)
		delete(document.historical, "timeline")
	}
	if corrections.RootCauses != nil {
		document.RootCauses = append([]CitedStatement(nil), (*corrections.RootCauses)...)
		delete(document.historical, "rootCauses")
	}
	if corrections.ContributingFactors != nil {
		document.ContributingFactors = append([]CitedStatement(nil), (*corrections.ContributingFactors)...)
		delete(document.historical, "contributingFactors")
	}
	if corrections.Resolution != nil {
		document.Resolution = *corrections.Resolution
	}
	if corrections.WhatWentWell != nil {
		document.WhatWentWell = append([]string(nil), (*corrections.WhatWentWell)...)
	}
	if corrections.WhatWentWrong != nil {
		document.WhatWentWrong = append([]string(nil), (*corrections.WhatWentWrong)...)
	}
	if corrections.DetectionGaps != nil {
		document.DetectionGaps = append([]string(nil), (*corrections.DetectionGaps)...)
	}
	if corrections.ActionItems != nil {
		document.ActionItems = append([]ActionItem(nil), (*corrections.ActionItems)...)
		delete(document.historical, "actionItems")
	}
	if corrections.OpenQuestions != nil {
		document.OpenQuestions = append([]string(nil), (*corrections.OpenQuestions)...)
	}
	return document
}
