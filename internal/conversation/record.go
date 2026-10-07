package conversation

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
)

type Source string

const (
	SourceWeb   Source = "web"
	SourceSlack Source = "slack"
)

func (s Source) String() string {
	switch s {
	case SourceWeb:
		return string(SourceWeb)
	case SourceSlack:
		return string(SourceSlack)
	default:
		return "unrecognised"
	}
}

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

type Role int16

const (
	RoleUser Role = iota + 1
	RoleAssistant
)

func (r Role) String() string {
	switch r {
	case RoleUser:
		return "user"
	case RoleAssistant:
		return "assistant"
	default:
		return "unrecognised"
	}
}

const (
	MaxSubjectLength      = 512
	MaxMessageTextLength  = 8192
	MaxActorIDLength      = 256
	MaxActorDisplayLength = 256
)

var (
	ErrUnknown         = errors.New("conversation unknown")
	ErrIncidentUnknown = errors.New("incident unknown")
	ErrClosed          = errors.New("conversation closed")
	ErrSourceMismatch  = errors.New("authenticated Messages cannot be appended to a Slack Conversation")
	ErrBadCursor       = errors.New("after is not a page position from a previous response")
	ErrQueueFull       = errors.New("this organization has too much work waiting")
)

type Conversation struct {
	ID             uuid.UUID
	OrgID          string
	IncidentID     uuid.UUID
	Source         Source
	Subject        string
	State          State
	CreatedBy      string
	CreatedAt      time.Time
	LastActivityAt time.Time
}

type Message struct {
	WindowFrom, WindowUntil time.Time
	Sequence                int64
	Role                    Role
	ActorID                 string
	ActorDisplay            string
	Text                    string
	SourceReference         string
	InvestigationID         uuid.UUID
	CreatedAt               time.Time
}

func (m Message) Queued() bool { return m.InvestigationID == uuid.Nil }

type Turn struct {
	InvestigationID uuid.UUID
	Ordinal         int
	Status          string
	Answer          string
	StoppedBy       string
	Error           string
	CreatedAt       time.Time
	ConcludedAt     time.Time
}

type NewConversation struct {
	IncidentID uuid.UUID
	Source     Source
	Subject    string
	CreatedBy  string
}

type NewMessage struct {
	Window       *Window
	Role         Role
	ActorID      string
	ActorDisplay string
	Text         string
}

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

type List struct {
	Conversations []Conversation
	Next          string
}

type Detail struct {
	Conversation Conversation
	Messages     []Message
	Turns        []Turn
	TurnsNext    string
}

type TurnPage struct {
	Turns []Turn
	Next  string
}

type Store interface {
	OpenConversation(ctx context.Context, who authz.Principal, org uuid.UUID,
		wanted NewConversation) (Conversation, error)
	Conversation(ctx context.Context, org uuid.UUID,
		id uuid.UUID) (Conversation, error)
	QueryConversations(ctx context.Context, who authz.Principal,
		org uuid.UUID, page Page) (List, error)
	ConversationDetail(ctx context.Context, org uuid.UUID, id uuid.UUID,
		messages int) (Detail, error)
	ConversationTurns(ctx context.Context, org uuid.UUID, id uuid.UUID,
		limit int, cursor string) (TurnPage, error)
	AppendMessageAndOpenTurn(
		ctx context.Context,
		who authz.Principal,
		org uuid.UUID,
		id uuid.UUID,
		said NewMessage,
		lead time.Duration,
		maxPending int) (Message, Turn, bool, error)
}

func Bounded(text string, limit int) string { return boundedRunes(text, limit) }
