package investigation

import (
	"context"
	"net/http"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/api/sse"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/conversation"
)

type EventType int16

const (
	EventStarted       EventType = 1
	EventProgress      EventType = 2
	EventToolStarted   EventType = 3
	EventToolCompleted EventType = 4
	EventConcluded     EventType = 6
	EventFailed        EventType = 7
	EventCancelled     EventType = 9
)

func (t EventType) String() string {
	switch t {
	case EventStarted:
		return "started"
	case EventProgress:
		return "progress"
	case EventToolStarted:
		return "tool_started"
	case EventToolCompleted:
		return "tool_completed"
	case EventConcluded:
		return "concluded"
	case EventFailed:
		return "failed"
	case EventCancelled:
		return "cancelled"
	default:
		return "unrecognised"
	}
}

type Event struct {
	Sequence int64
	At       time.Time
	Type     EventType
	Payload  map[string]any
}

const (
	eventTextBound    = 512
	maxRunErrorLength = 1024
)

func bounded(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}

func (h Handlers) streamEvents(writer http.ResponseWriter, request *http.Request) {
	organization, id, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	sse.Stream(writer, request, h.StreamContext, h.Resolve, authz.InvestigationRead,
		func(ctx context.Context) (conversation.ExecutionSnapshot, error) {
			return h.Store.InvestigationExecutionSnapshot(ctx, organization, id)
		}, h.fail)
}
