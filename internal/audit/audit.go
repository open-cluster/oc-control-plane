package audit

import (
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

var ErrWriteFailed = errors.New("the audit record could not be written")

const (
	maxAuditTextLength     = 512
	maxAuditMetadataLength = 128
	maxAuditDetailEntries  = 32
)

type ActorKind int16

const (
	ActorUser   ActorKind = 1
	ActorSystem ActorKind = 3
)

func (k ActorKind) String() string {
	switch k {
	case ActorUser:
		return "user"
	case ActorSystem:
		return "system"
	default:
		return "unrecognised"
	}
}

type Outcome int16

const (
	OutcomeAllowed Outcome = 1
	OutcomeDenied  Outcome = 2
	OutcomeFailed  Outcome = 3
)

func (o Outcome) String() string {
	switch o {
	case OutcomeAllowed:
		return "allowed"
	case OutcomeDenied:
		return "denied"
	case OutcomeFailed:
		return "failed"
	default:
		return "unrecognised"
	}
}

type Actor struct {
	Kind        ActorKind
	ID          string
	DisplayName string
}

func System(what string) Actor {
	return Actor{Kind: ActorSystem, DisplayName: what}
}

type TargetKind string

const (
	TargetIntegration   TargetKind = "integration"
	TargetUser          TargetKind = "user"
	TargetInvestigation TargetKind = "investigation"
	TargetConversation  TargetKind = "conversation"
	TargetIncident      TargetKind = "incident"
	TargetPostmortem    TargetKind = "postmortem"
	TargetRelay         TargetKind = "relay"
	TargetSession       TargetKind = "session"
	TargetOrganization  TargetKind = "organization"
	TargetRoute         TargetKind = "route"
)

type Target struct {
	Kind TargetKind
	ID   string
}

type Event struct {
	Organization  string
	Actor         Actor
	Action        Action
	Target        Target
	Outcome       Outcome
	SourceAddress string
	RequestID     string
	OccurredAt    time.Time
	Detail        Detail
}

func (e Event) Bounded() Event {
	e.Actor.DisplayName = truncate(e.Actor.DisplayName, maxAuditTextLength)
	e.Actor.ID = truncate(e.Actor.ID, maxAuditTextLength)
	e.Target.ID = truncate(e.Target.ID, maxAuditTextLength)
	e.SourceAddress = truncate(e.SourceAddress, maxAuditMetadataLength)
	e.RequestID = truncate(e.RequestID, maxAuditMetadataLength)
	e.Detail = e.Detail.Safe()
	return e
}

func truncate(value string, limit int) string {
	value = strings.TrimSpace(value)
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	const marker = "..."
	markerLength := utf8.RuneCountInString(marker)
	runes := []rune(value)
	if limit <= markerLength {
		return string(runes[:limit])
	}
	return string(runes[:limit-markerLength]) + marker
}

type Page struct {
	Limit int
	After string
}

type List struct {
	Events []Recorded
	Next   string
}

type Recorded struct {
	Event
	ID string
}
