// Package conversation owns the multi-turn context a person talks to: an
// organization-scoped record holding the messages said in it, optionally about one
// incident episode, and the investigations its turns opened.
//
// What it deliberately does NOT own is the investigation. A turn opens one, and one is
// exactly what it always was — a bounded answer with its own offered sources, tool runs,
// findings, citations, ceilings and spend. This package carries the CONTINUITY between
// turns and nothing else.
//
// Like every other capability here it is a leaf: it declares what it needs from durable
// state as an interface in its own vocabulary and never imports another capability. A
// turn is therefore a Turn — a small projection of the investigation it names — rather
// than the investigation record itself, so the two domains stay separable.
package conversation

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// Surface is where the person is talking from. Persisted as the integer in the column;
// the values are frozen. A later surface — a Slack thread, a DM — adds its own value in
// its own migration.
type Surface int16

const (
	SurfaceWeb Surface = iota + 1
	// SurfaceSlack is a thread in a customer's own workspace. The Conversation is the
	// same record either way: OpenCluster is not a second AI system inside Slack, it is
	// the same one reached from somewhere else.
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

// ActorKind distinguishes an OpenCluster principal from an identity belonging to an
// external surface. Persisted; frozen. It exists because a Slack participant is not a
// principal and never will be, and recording their surface identity as though it were
// one would make an audit answer the wrong question.
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
	// ErrUnknown reports a conversation this organization does not have. It is the
	// answer for a conversation belonging to another tenant too: a caller must not learn
	// that an identifier exists somewhere they cannot reach.
	ErrUnknown = errors.New("conversation unknown")
	// ErrEpisodeUnknown reports an episode this organization does not have.
	ErrEpisodeUnknown = errors.New("episode unknown")
	// ErrClosed reports a message sent to a conversation that has been closed.
	ErrClosed = errors.New("conversation closed")
	// ErrBadCursor reports a page position that did not come from a previous response.
	ErrBadCursor = errors.New("after is not a page position from a previous response")
	// ErrQueueFull reports an organization whose unclaimed turns are already at its
	// ceiling. Refusing plainly is the alternative to a queue that grows without bound.
	ErrQueueFull = errors.New("this organization has too many investigations waiting")
)

// Conversation is the record: who opened it, what it is about, and when it last moved.
type Conversation struct {
	ID    uuid.UUID
	OrgID string
	// EpisodeID is the incident episode this conversation is about, zero when it names
	// none. Several conversations may name one episode — two people narrowing the same
	// incident separately is what that is for — and they share only what the episode
	// itself holds, never each other's messages.
	EpisodeID uuid.UUID
	Surface   Surface
	Subject   string
	State     State
	CreatedBy string
	CreatedAt time.Time
	// LastActivityAt is what the listing orders by. A conversation is found by when it
	// last moved, not by when it was opened.
	LastActivityAt time.Time
}

// Message is one thing said, at its position in the conversation. The text is untrusted
// for its whole life: it reaches a model as evidence about what somebody typed, never as
// an instruction.
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
	EpisodeID uuid.UUID
	Surface   Surface
	Subject   string
	CreatedBy string
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

// Page is a position in a listing, with what the caller narrowed it by.
//
// Narrowing happens in the DATABASE. A listing that answered everything and left a console
// to filter it would be one whose paging lies — page one of a hundred conversations
// filtered down to three is not three conversations, it is however many of the first
// hundred matched.
type Page struct {
	Limit int
	After string
	// Search narrows by subject, case-insensitively. Empty means no narrowing.
	Search string
	// Episode narrows to the conversations about one incident episode, which is how two
	// people looking at the same incident find each other's. Zero means no narrowing.
	Episode uuid.UUID
	// State narrows to open or closed conversations. Zero means either.
	State State
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

// Store is everything this capability needs from durable state.
//
// Opening a turn is here rather than on the investigation side because THIS is where the
// single-writer invariant is decided: a message arriving while a turn runs must be
// queued, not start a second agent, and the caller has to be able to tell the two
// outcomes apart.
type Store interface {
	// OpenConversation records one and audits the act.
	OpenConversation(ctx context.Context, who authz.Principal, org tenancy.Organization,
		wanted NewConversation) (Conversation, error)
	// Conversation reads one, scoped to the tenant.
	Conversation(ctx context.Context, org tenancy.Organization,
		id uuid.UUID) (Conversation, error)
	// QueryConversations reports a page, most recently active first.
	QueryConversations(ctx context.Context, who authz.Principal,
		org tenancy.Organization, page Page) (List, error)
	// ConversationDetail reads one with its messages and its turns.
	ConversationDetail(ctx context.Context, org tenancy.Organization, id uuid.UUID,
		messages int) (Detail, error)
	// AppendMessage writes one message at the next sequence and stamps the
	// conversation's activity. It does NOT open a turn.
	AppendMessage(ctx context.Context, who authz.Principal, org tenancy.Organization,
		id uuid.UUID, said NewMessage) (Message, error)
	// OpenTurn opens the next turn from whatever messages are queued, and attaches them
	// to it. The second return is false when a turn is already running for this
	// conversation — the single-writer invariant refusing, which is not an error: the
	// messages stay queued and the drain at the running turn's terminal takes them up.
	// It is also false when nothing is queued, which a drain finding an empty queue is.
	//
	// lead is how far before the episode began the turn's window reaches back. A
	// conversation naming no episode gets a window of that length ending now.
	OpenTurn(ctx context.Context, org tenancy.Organization, id uuid.UUID,
		lead time.Duration) (Turn, bool, error)
	// WaitingTurns counts this organization's turns that are open and unclaimed. It is
	// the queue, and the ceiling on it is what keeps overload boring: work above the
	// ceiling is refused with a plain reason rather than queued without bound.
	WaitingTurns(ctx context.Context, org tenancy.Organization) (int, error)
}

// Bounded cuts text to what a column will hold, at a rune boundary.
//
// Exported for the surfaces that receive text they did not ask a person to type. The console
// refuses an over-long message, which is right there: somebody typed it, they are standing at
// the keyboard, and telling them is better than silently truncating what they wrote. A chat
// surface has nobody to tell — the message is already sent, refusing it would drop a question
// somebody asked in good faith, and the vendor's own limits differ from ours.
func Bounded(text string, limit int) string { return boundedRunes(text, limit) }
