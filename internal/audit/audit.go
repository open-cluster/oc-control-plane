// Package audit holds the vocabulary of the record: who did what, to which tenant's what,
// when, from where, and whether it was allowed.
package audit

import (
	"errors"
	"strings"
	"time"
)

var ErrWriteFailed = errors.New("the audit record could not be written")

const (
	MaxTargetIDLength    = 256
	MaxDisplayNameLength = 256
	MaxSourceAddrLength  = 128
	MaxRequestIDLength   = 128
	MaxDetailValueLength = 512
	MaxDetailEntries     = 32
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

// Outcome is what happened. A denial is recorded as loudly as a success: credential probing is
// only visible if the refusals are on the record too.
type Outcome int16

const (
	// OutcomeAllowed is an act that was authorized and did what it said.
	OutcomeAllowed Outcome = 1
	// OutcomeDenied is an act authorization refused.
	OutcomeDenied Outcome = 2
	// OutcomeFailed is an act that was authorized and then did not work.
	OutcomeFailed Outcome = 3
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

// Actor is who acted, as the record will hold them forever.
//
// DisplayName is captured at the time of writing rather than joined at read time on purpose. A
// user who is renamed, or deleted, must not silently rewrite what the record says about what
// they did — an audit trail that changes when a row elsewhere changes is not a record.
type Actor struct {
	Kind ActorKind
	// ID is the user or service account identifier, and is empty for ActorSystem.
	ID string
	// DisplayName is what the actor was called when this was written.
	DisplayName string
}

// System is the actor for work the control plane does on its own behalf.
func System(what string) Actor {
	return Actor{Kind: ActorSystem, DisplayName: what}
}

// TargetKind names the sort of thing that was acted on. It is text rather than an integer
// because it is read during an incident and a number would have to be looked up.
type TargetKind string

const (
	TargetIntegration     TargetKind = "integration"
	TargetInvestigation   TargetKind = "investigation"
	TargetConversation    TargetKind = "conversation"
	TargetIncident        TargetKind = "incident"
	TargetPostmortem      TargetKind = "postmortem"
	TargetRelay           TargetKind = "relay"
	TargetMembership      TargetKind = "membership"
	TargetSession         TargetKind = "session"
	TargetOrganization    TargetKind = "organization"
	TargetRoute           TargetKind = "route"
	TargetWebhookDelivery TargetKind = "webhook_delivery"
)

// Target is what was acted on.
type Target struct {
	Kind TargetKind
	// ID identifies the thing within its organization. It is a string rather than a UUID
	// because not every target has one — a refused route names the pattern that was reached
	// for, and an organization names itself.
	ID string
}

// Event is one entry in the record. It is append-only: nothing updates one, the database
// refuses an UPDATE and a DELETE outright, and there is deliberately no function here that
// would build a modification.
type Event struct {
	// Organization is the tenant the act belongs to. Every event has one; a cross-tenant read
	// is recorded against the tenant that was read, not against the reader's own.
	Organization string
	Actor        Actor
	Action       Action
	Target       Target
	Outcome      Outcome
	// SourceAddress is where the request came from. It is kept alongside the actor rather than
	// instead of it: the actor says who, and this says from where, and an offboarding
	// investigation needs both.
	SourceAddress string
	// RequestID ties this event to the log lines for the same request.
	RequestID string
	// OccurredAt is when. Zero means the database's own clock, which is what an ordinary write
	// uses so that ordering within a database is that database's clock throughout.
	OccurredAt time.Time
	// Detail is structured context — the previous and new value of a changed setting, the
	// reason a request was denied. Never a credential and never raw source content.
	Detail Detail
}

// Bounded returns the event with every attacker-influenced string cut to its limit and the
// detail sanitised. Storage calls it on the way in, so no call site has to remember.
func (e Event) Bounded() Event {
	e.Actor.DisplayName = truncate(e.Actor.DisplayName, MaxDisplayNameLength)
	e.Actor.ID = truncate(e.Actor.ID, MaxTargetIDLength)
	e.Target.ID = truncate(e.Target.ID, MaxTargetIDLength)
	e.SourceAddress = truncate(e.SourceAddress, MaxSourceAddrLength)
	e.RequestID = truncate(e.RequestID, MaxRequestIDLength)
	e.Detail = e.Detail.Safe()
	return e
}

func truncate(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}

// Page is a position in the record, so an auditor reading a long history pages through it
// without losing or repeating an entry.
type Page struct {
	Limit int
	After string
}

// List is a page of events, newest first.
type List struct {
	Events []Recorded
	Next   string
}

// Recorded is an Event as it came back out, with the identity and time the database gave it.
type Recorded struct {
	Event
	ID string
}
