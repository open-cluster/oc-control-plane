package conversation

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
)

// Surface is where the person is talking from. Persisted as the integer in the column;
// the values are frozen. A later surface — a Slack thread, a DM — adds its own value in its own migration.
type Surface int16

const (
	SurfaceWeb Surface = iota + 1
	SurfaceSlack
)

func (s Surface) String() string {
	switch s {
	case SurfaceWeb:
		return "web"
	case SurfaceSlack:
		return "slack"
	default:
		return "unrecognised"
	}
}

// State is whether a conversation still takes messages. Persisted; frozen.
type State int16

const (
	StateOpen State = iota + 1
	StateClosed
)

func (s State) String() string {
	switch s {
	case StateOpen:
		return "open"
	case StateClosed:
		return "closed"
	default:
		return "unrecognised"
	}
}

// Role is who a message came from. Persisted; frozen.
type Role int16

const (
	RolePerson Role = iota + 1
	RoleAgent
)

func (r Role) String() string {
	switch r {
	case RolePerson:
		return "person"
	case RoleAgent:
		return "agent"
	default:
		return "unrecognised"
	}
}

type ActorKind int16

const (
	ActorPrincipal ActorKind = iota + 1
	ActorExternal
)

func (a ActorKind) String() string {
	switch a {
	case ActorPrincipal:
		return "principal"
	case ActorExternal:
		return "external"
	default:
		return "unrecognised"
	}
}

// The record's own bounds, mirroring the schema's CHECK constraints, which count
// characters as these do.
const (
	MaxSubjectLength      = 512
	MaxMessageTextLength  = 8192
	MaxActorIDLength      = 256
	MaxActorDisplayLength = 256
)

// Refusals this capability names.
var (
	ErrUnknown         = errors.New("conversation unknown")
	ErrIncidentUnknown = errors.New("incident unknown")
	ErrClosed          = errors.New("conversation closed")
	ErrBadCursor       = errors.New("after is not a page position from a previous response")
	ErrQueueFull       = errors.New("this organization has too much work waiting")
)

// Conversation is the record: who opened it, what it is about, and when it last moved.
type Conversation struct {
	ID    uuid.UUID
	OrgID string
	// IncidentID is the incident incident this conversation is about, zero when it names
	// none. Several conversations may name one incident — two people narrowing the same
	// incident separately is what that is for — and they share only what the incident
	// itself holds, never each other's messages.
	IncidentID uuid.UUID
	Surface    Surface
	Subject    string
	State      State
	CreatedBy  string
	CreatedAt  time.Time
	// LastActivityAt is what the listing orders by. A conversation is found by when it
	// last moved, not by when it was opened.
	LastActivityAt time.Time
}

// Message is one thing said, at its position in the Conversation. Its text never grants
// Integration or resource authority.
type Message struct {
	// Sequence is monotonic within the conversation, from one.
	Sequence     int64
	Role         Role
	ActorKind    ActorKind
	ActorID      string
	ActorDisplay string
	Text         string
	// SourceReference is a provider-authored navigation URL for this exact message.
	// Empty when the surface has none or its post-acceptance lookup did not succeed.
	SourceReference string
	// InvestigationID is the turn this message opened, or the turn that produced it.
	// Zero on a message that arrived while a turn was still running: that message is
	// QUEUED, and the drain at the next terminal boundary is what gives it a turn.
	InvestigationID uuid.UUID
	CreatedAt       time.Time
}

// Queued reports whether no turn has taken this message up yet.
func (m Message) Queued() bool { return m.InvestigationID == uuid.Nil }

// Turn is one investigation this conversation opened, projected into this domain's
// vocabulary. It is a PROJECTION and not the investigation record: reading the whole
// record is the investigation surface's own route, and duplicating it here would be two
// contracts for one thing.
type Turn struct {
	InvestigationID uuid.UUID
	// Ordinal is the turn's one-based position in the conversation.
	Ordinal int
	// Status is the investigation's lifecycle word — "running", "concluded", "failed" —
	// carried as the investigation surface renders it so the two never disagree.
	Status string
	// Answer is the direct reply, empty while the turn runs or when it carried none.
	Answer string
	// StoppedBy names the ceiling that forced the conclusion, empty when the model
	// concluded freely. Error says why a failed turn failed.
	StoppedBy   string
	Error       string
	CreatedAt   time.Time
	ConcludedAt time.Time
}

// NewConversation is what an open records.
type NewConversation struct {
	IncidentID uuid.UUID
	Surface    Surface
	Subject    string
	CreatedBy  string
}

// NewMessage is one thing to say.
type NewMessage struct {
	Role         Role
	ActorKind    ActorKind
	ActorID      string
	ActorDisplay string
	Text         string
}

// boundedRunes cuts text at a rune boundary inside the limit. Runes rather than bytes,
// because every bound in this package mirrors a column CHECK that counts characters.
func boundedRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}

type Page struct {
	Limit      int
	After      string
	Sort       string
	Descending bool
	Search     string
	Incident   uuid.UUID
	State      State
}

// List is a page of an organization's conversations, most recently active first.
type List struct {
	Conversations []Conversation
	Next          string
}

// Detail is one conversation with what happened in it.
type Detail struct {
	Conversation Conversation
	Messages     []Message
	Turns        []Turn
}

type Store interface {
	OpenConversation(ctx context.Context, who authz.Principal, org tenancy.Organization,
		wanted NewConversation) (Conversation, error)
	Conversation(ctx context.Context, org tenancy.Organization,
		id uuid.UUID) (Conversation, error)
	QueryConversations(ctx context.Context, who authz.Principal,
		org tenancy.Organization, page Page) (List, error)
	ConversationDetail(ctx context.Context, org tenancy.Organization, id uuid.UUID,
		messages int) (Detail, error)
	AppendMessageAndOpenTurn(
		ctx context.Context,
		who authz.Principal,
		org tenancy.Organization,
		id uuid.UUID,
		said NewMessage,
		lead time.Duration,
		maxPending int) (Message, Turn, bool, error)
}

// Bounded cuts text to what a column will hold, at a rune boundary.
func Bounded(text string, limit int) string { return boundedRunes(text, limit) }
