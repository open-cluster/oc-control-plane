package conversation

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// What this surface says on the wire. Kept apart from the handlers because it is a
// contract: a field renamed here is a client broken somewhere else.

type errorView struct {
	Error string `json:"error"`
}

type conversationView struct {
	ID        string `json:"id"`
	Subject   string `json:"subject"`
	Surface   string `json:"surface"`
	State     string `json:"state"`
	EpisodeID string `json:"episodeId,omitempty"`
	CreatedBy string `json:"createdBy,omitempty"`
	CreatedAt string `json:"createdAt"`
	// LastActivityAt is what the listing is ordered by, so a client rendering the order
	// can render the reason for it.
	LastActivityAt string `json:"lastActivityAt"`
}

type messageView struct {
	Sequence int64  `json:"sequence"`
	Role     string `json:"role"`
	// ActorKind says whether the actor is an OpenCluster principal or an identity that
	// belongs to some other surface, so a client never renders one as the other.
	ActorKind       string `json:"actorKind"`
	ActorID         string `json:"actorId,omitempty"`
	ActorDisplay    string `json:"actorDisplay,omitempty"`
	Text            string `json:"text"`
	SourceReference string `json:"sourceReference,omitempty"`
	// InvestigationID is the turn this message opened or came from. Absent while the
	// message is still queued, which is what a client renders as "waiting".
	InvestigationID string `json:"investigationId,omitempty"`
	CreatedAt       string `json:"createdAt"`
}

type turnView struct {
	InvestigationID string `json:"investigationId"`
	Turn            int    `json:"turn"`
	Status          string `json:"status"`
	Answer          string `json:"answer,omitempty"`
	StoppedBy       string `json:"stoppedBy,omitempty"`
	Error           string `json:"error,omitempty"`
	CreatedAt       string `json:"createdAt"`
	ConcludedAt     string `json:"concludedAt,omitempty"`
}

// detailView is one conversation with what was said in it and the turns it opened.
type detailView struct {
	conversationView
	Messages []messageView `json:"messages"`
	Turns    []turnView    `json:"turns"`
}

// messageAcceptedView is the answer to posting a message. It always reports the message
// that was recorded, and reports a turn only when one actually opened — a message
// accepted while the agent is working is queued, and saying so is the difference between
// "we took it" and "we are answering it now".
type messageAcceptedView struct {
	Message messageView `json:"message"`
	Turn    *turnView   `json:"turn,omitempty"`
	// Queued is true when the message was accepted and no turn opened for it, because
	// one is already running. It is drained into the next turn at that one's end.
	Queued bool `json:"queued"`
}

// openedView is the answer to opening a conversation WITH a first message: the
// conversation, and the same message answer that posting one to an existing conversation
// gives. One shape for one meaning, so a client that has learned the message answer has
// already learned this one.
type openedView struct {
	conversationView
	messageAcceptedView
}

func conversationViewOf(found Conversation) conversationView {
	view := conversationView{
		ID:             found.ID.String(),
		Subject:        found.Subject,
		Surface:        found.Surface.String(),
		State:          found.State.String(),
		CreatedBy:      found.CreatedBy,
		CreatedAt:      stamp(found.CreatedAt),
		LastActivityAt: stamp(found.LastActivityAt),
	}
	if found.EpisodeID != uuid.Nil {
		view.EpisodeID = found.EpisodeID.String()
	}
	return view
}

func messageViewOf(message Message) messageView {
	view := messageView{
		Sequence:        message.Sequence,
		Role:            message.Role.String(),
		ActorKind:       message.ActorKind.String(),
		ActorID:         message.ActorID,
		ActorDisplay:    message.ActorDisplay,
		Text:            message.Text,
		SourceReference: message.SourceReference,
		CreatedAt:       stamp(message.CreatedAt),
	}
	if message.InvestigationID != uuid.Nil {
		view.InvestigationID = message.InvestigationID.String()
	}
	return view
}

func turnViewOf(turn Turn) turnView {
	view := turnView{
		InvestigationID: turn.InvestigationID.String(),
		Turn:            turn.Ordinal,
		Status:          turn.Status,
		Answer:          turn.Answer,
		StoppedBy:       turn.StoppedBy,
		Error:           turn.Error,
		CreatedAt:       stamp(turn.CreatedAt),
	}
	if !turn.ConcludedAt.IsZero() {
		view.ConcludedAt = stamp(turn.ConcludedAt)
	}
	return view
}

func detailViewOf(detail Detail) detailView {
	view := detailView{
		conversationView: conversationViewOf(detail.Conversation),
		Messages:         make([]messageView, 0, len(detail.Messages)),
		Turns:            make([]turnView, 0, len(detail.Turns)),
	}
	for _, message := range detail.Messages {
		view.Messages = append(view.Messages, messageViewOf(message))
	}
	for _, turn := range detail.Turns {
		view.Turns = append(view.Turns, turnViewOf(turn))
	}
	return view
}

// stamp renders a timestamp in the one format this surface uses.
func stamp(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.UTC().Format(time.RFC3339Nano)
}

func writeJSON(writer http.ResponseWriter, status int, body any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(body)
}
