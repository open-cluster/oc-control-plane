package investigation

import "github.com/google/uuid"

// ConversationOrigin is the verified provider resource that constrains a Conversation's tools.
type ConversationOrigin struct {
	IntegrationID uuid.UUID
	Channel       string
	Thread        string
}
