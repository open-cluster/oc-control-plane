package conversation

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/execution"
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
	MaxTitleLength       = 512
	MaxMessageTextLength = 8192
	MaxAuthorIDLength    = 256
	MaxAuthorNameLength  = 256
)

var (
	ErrUnknown               = errors.New("conversation unknown")
	ErrIncidentUnknown       = errors.New("incident unknown")
	ErrBadCursor             = errors.New("after is not a page position from a previous response")
	ErrQueueFull             = errors.New("this organization has too much work waiting")
	ErrClientMessageConflict = errors.New("client message ID already names different input")
	ErrLegacyHistory         = errors.New("this Conversation is read-only; create a new Conversation to continue")
	ErrChatUnavailable       = errors.New("this deployment has no model provider configured for Chat")
)

type Conversation struct {
	ID             uuid.UUID
	IncidentID     uuid.UUID
	Source         Source
	Title          string
	CreatedBy      string
	CreatedAt      time.Time
	LastActivityAt time.Time
}

type Message struct {
	WindowFrom, WindowUntil time.Time
	Sequence                int64
	Role                    Role
	AuthorID                string
	AuthorName              string
	Text                    string
	SourceReference         string
	InvestigationID         uuid.UUID
	ExecutionID             uuid.UUID
	CreatedAt               time.Time
}

func (m Message) Queued() bool { return m.InvestigationID == uuid.Nil }

type Turn struct {
	InvestigationID uuid.UUID
	Ordinal         int
	Status          string
	StoppedBy       string
	Error           string
	CreatedAt       time.Time
	ConcludedAt     time.Time
}

type NewConversation struct {
	IncidentID uuid.UUID
	Source     Source
	Title      string
	CreatedBy  string
}

type NewMessage struct {
	Window     *Window
	Role       Role
	AuthorID   string
	AuthorName string
	Text       string
}

type AcceptedMessage struct {
	Message     Message
	ExecutionID uuid.UUID
	Status      string
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

type MessagePage struct {
	Messages []Message
	Next     string
}

type Store interface {
	OpenConversation(ctx context.Context, who authz.Principal, org uuid.UUID,
		wanted NewConversation) (Conversation, error)
	QueryConversations(ctx context.Context, who authz.Principal,
		org uuid.UUID, page Page) (List, error)
	WebConversation(ctx context.Context, org, id uuid.UUID) (Conversation, error)
	RenameConversation(ctx context.Context, who authz.Principal, org, id uuid.UUID, title string) (Conversation, error)
	DeleteConversation(ctx context.Context, who authz.Principal, org, id uuid.UUID) error
	ConversationMessages(ctx context.Context, org, id uuid.UUID, limit int, cursor string) (MessagePage, error)
	ConversationTurns(ctx context.Context, org uuid.UUID, id uuid.UUID,
		limit int, cursor string) (TurnPage, error)
	AdmitConversationMessage(ctx context.Context, who authz.Principal, org, id, clientMessageID uuid.UUID, input NewMessage, replayOnly bool) (AcceptedMessage, error)
	ChatExecutionSnapshot(ctx context.Context, org, chat, id uuid.UUID) (execution.Snapshot, error)
	CancelChatExecution(ctx context.Context, who authz.Principal, org, chat, id uuid.UUID) (execution.Snapshot, error)
}

func Bounded(text string, limit int) string { return boundedRunes(text, limit) }
