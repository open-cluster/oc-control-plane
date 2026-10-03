package investigation

import "github.com/google/uuid"

type ConversationOrigin struct {
	IntegrationID uuid.UUID
	Channel       string
	Thread        string
}
