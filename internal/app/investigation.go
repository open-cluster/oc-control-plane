package app

import (
	"context"
	"encoding/json"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/ai"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/seal"
	storage "github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

const ollamaContextSize = 32_768

func prepareAgentExecution(
	db *storage.Database, catalog integrations.Catalog, sealer seal.Sealer, chatModel model.ChatModel,
) PrepareExecution {
	return func(ctx context.Context, org uuid.UUID, execution storage.AgentExecution) (ExecutionProfile, error) {
		profile := ExecutionProfile{Config: ai.Config{
			Name:         "chat",
			Instructions: ai.ChatInstructions,
			Model:        chatModel,
		}}
		profile.Input = func(ctx context.Context) (string, error) {
			return db.AgentChatInput(ctx, org, execution)
		}
		if execution.InvestigationID != uuid.Nil {
			profile.Name = "investigator"
			profile.Instructions = ai.InvestigationInstructions
			profile.Input = func(ctx context.Context) (string, error) {
				return investigationInput(ctx, db, org, execution)
			}
		}
		if err := profile.UseIntegrationTools(ctx, db, catalog, sealer, org, execution); err != nil {
			return ExecutionProfile{}, err
		}
		return profile, nil
	}
}

func investigationInput(ctx context.Context, db *storage.Database, org uuid.UUID, execution storage.AgentExecution) (string, error) {
	id := execution.InvestigationID
	opened, err := db.Investigation(ctx, org, id)
	if err != nil {
		return "", err
	}
	input := struct {
		InvestigationID uuid.UUID                       `json:"investigationId"`
		Subject         string                          `json:"subject"`
		WindowFrom      time.Time                       `json:"windowFrom"`
		WindowUntil     time.Time                       `json:"windowUntil"`
		Incident        json.RawMessage                 `json:"incident,omitempty"`
		Messages        []investigation.AssignedMessage `json:"messages,omitempty"`
	}{
		InvestigationID: id,
		Subject:         opened.Subject,
		WindowFrom:      opened.WindowFrom,
		WindowUntil:     opened.WindowUntil,
	}
	if opened.IncidentID != uuid.Nil {
		trigger, err := db.TriggerIncident(ctx, org, opened.IncidentID)
		if err != nil {
			return "", err
		}
		input.Incident, err = json.Marshal(trigger)
		if err != nil {
			return "", err
		}
		if len(input.Incident) > 16<<10 {
			trigger.Labels = nil
			trigger.Annotations = nil
			trigger.GeneratorURL = ""
			input.Incident, err = json.Marshal(trigger)
			if err != nil {
				return "", err
			}
		}
	}
	if execution.HasAssignedInput() {
		input.Messages, err = db.InvestigationMessages(ctx, org, opened.ConversationID, id)
		if err != nil {
			return "", err
		}
	}
	raw, err := json.Marshal(input)
	return "Untrusted Investigation task data:\n" + string(raw), err
}
