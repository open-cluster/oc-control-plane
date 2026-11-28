package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/seal"
	storage "github.com/open-cluster/oc-control-plane/internal/store/postgres"
	"github.com/open-cluster/oc-control-plane/internal/telemetry"
)

const integrationToolTimeout = 30 * time.Second

var errToolScope = errors.New("execution is no longer authorized to use Tools")

type offeredOperation struct {
	definition integrations.Tool
	sources    []integrations.Integration
}

// UseIntegrationTools configures the native Toolkit for the current claim.
func (profile *ExecutionProfile) UseIntegrationTools(
	ctx context.Context, database *storage.Database, catalog integrations.Catalog, sealer seal.Sealer,
	org uuid.UUID, execution storage.AgentExecution,
) error {
	_, origin, err := database.AgentToolScope(ctx, org, execution)
	if err != nil {
		return errToolScope
	}
	scope, err := loadToolOrigin(ctx, database, org, origin)
	if err != nil {
		return err
	}
	candidates, err := database.InvestigationCandidates(ctx, org)
	if err != nil {
		return errors.New("could not read Integrations for the Toolkit")
	}
	operations := make(map[string]offeredOperation)
	for _, candidate := range candidates {
		definition, ok := catalog.Lookup(candidate.Provider)
		if !ok {
			continue
		}
		for _, declared := range integrations.SupportedTools(definition, candidate) {
			if !declared.ReadOnly || !declared.AllowedForOrigin(candidate, scope) {
				continue
			}
			operation, exists := operations[declared.Name]
			if !exists {
				operation.definition = declared
			} else {
				operation.definition.ConcurrencySafe = operation.definition.ConcurrencySafe && declared.ConcurrencySafe
			}
			operation.sources = append(operation.sources, integrations.Integration{
				ID: candidate.ID, Name: candidate.Name,
			})
			operations[declared.Name] = operation
		}
	}
	var tools []tool.Tool
	if execution.ConversationID != uuid.Nil && origin == nil {
		tools = append(tools, tool.AskUserTool())
	}
	for _, name := range slices.Sorted(maps.Keys(operations)) {
		operation := operations[name]
		definition := operation.definition.Definition().WithInputEnvelope(operation.sources)
		schema, err := json.Marshal(definition.InputSchema)
		if err != nil {
			return errors.New("could not prepare the Tool input schema")
		}
		registered := tool.NewFunctionTool(name, definition.Description, schema, func(ctx context.Context, input map[string]any) (any, error) {
			id := toolIntegration(operation.sources, input)
			if id == uuid.Nil {
				return nil, errors.New("select an Integration offered for this Tool")
			}
			return runIntegrationTool(ctx, database, catalog, sealer, org, execution, id, name, input)
		})
		registered.ReadOnly = true
		registered.ConcurrencySafe = operation.definition.ConcurrencySafe
		tools = append(tools, registered)
	}
	profile.Toolkit = tool.NewToolkit(tools...)
	profile.ToolConcurrency = 2
	if profile.Permissions == nil {
		profile.Permissions = permission.NewContext(permission.ModeExplore)
	}
	return nil
}

func toolIntegration(sources []integrations.Integration, input map[string]any) uuid.UUID {
	value, supplied := input["integrationId"]
	if len(sources) == 1 && !supplied {
		return sources[0].ID
	}
	selector, ok := value.(string)
	if !ok {
		return uuid.Nil
	}
	id, err := uuid.Parse(selector)
	if err != nil || !slices.ContainsFunc(sources, func(source integrations.Integration) bool {
		return source.ID == id
	}) {
		return uuid.Nil
	}
	return id
}

func loadToolOrigin(
	ctx context.Context, database *storage.Database, org uuid.UUID, origin *investigation.ConversationOrigin,
) (*integrations.Integration, error) {
	if origin == nil {
		return nil, nil
	}
	integration, err := database.Integration(ctx, org, origin.IntegrationID)
	if err != nil || integration.Disconnected || integration.Status != integrations.StatusVerified ||
		integration.Installation == nil || !integration.Installation.Key.Complete() {
		return nil, errToolScope
	}
	return &integration, nil
}

func runIntegrationTool(
	ctx context.Context, database *storage.Database, catalog integrations.Catalog, sealer seal.Sealer,
	org uuid.UUID, execution storage.AgentExecution, integrationID uuid.UUID, name string, input map[string]any,
) (any, error) {
	arguments, ok := input["input"].(map[string]any)
	if !ok || arguments == nil {
		return nil, errors.New("expected an object for Tool input")
	}
	purpose, _ := input["purpose"].(string)
	window, origin, err := database.AgentToolScope(ctx, org, execution)
	if err != nil || window.From.IsZero() || !window.From.Before(window.Until) {
		return nil, errToolScope
	}
	scope, err := loadToolOrigin(ctx, database, org, origin)
	if err != nil {
		return nil, err
	}
	integration, err := database.Integration(ctx, org, integrationID)
	if err != nil {
		return nil, errToolScope
	}
	definition, known := catalog.Lookup(integration.Provider)
	if !known {
		return nil, errToolScope
	}
	available := integrations.SupportedTools(definition, integration)
	selected := slices.IndexFunc(available, func(declared integrations.Tool) bool {
		return declared.Name == name && declared.ReadOnly && declared.AllowedForOrigin(integration, scope)
	})
	if selected < 0 {
		return nil, errToolScope
	}
	declared := available[selected]
	if definition.RequiresRelay {
		status, err := database.IntegrationRelayStatus(ctx, org, integration.RelayID)
		if err != nil || !status.Connected {
			return nil, errors.New("the Integration Relay is unavailable")
		}
		for _, grant := range declared.RequiredGrants {
			if !slices.Contains(status.Capabilities, grant) {
				return nil, errors.New("the Integration Relay no longer offers this Tool")
			}
		}
	}
	var credential string
	if len(integration.CredentialSealed) > 0 {
		if err := database.RecordCredentialUnseal(ctx, org, integration.ID, "agent Tool"); err != nil {
			return nil, errors.New("credential access could not be audited")
		}
		credential, err = sealer.Open(integration.CredentialSealed, integrations.CredentialBinding(integration.ID))
		if err != nil {
			return nil, errors.New("the Integration credential could not be opened")
		}
	}

	run := investigation.ToolRun{
		IntegrationID: integration.ID,
		Tool:          name,
		Arguments:     arguments,
		Purpose:       purpose,
		WindowFrom:    window.From,
		WindowUntil:   window.Until,
	}
	investigation.RedactToolInput(&run, credential)
	access := &storage.AgentToolAccess{
		Integration: integration,
		Origin:      origin,
	}
	run.ID, err = database.BeginAgentToolRun(ctx, org, execution, run, access)
	if err != nil {
		return nil, errors.New("could not admit the Tool Run")
	}
	request := integrations.ToolRequest{
		ExecutionID:         execution.ID,
		ExecutionLeaseToken: execution.LeaseToken,
		ToolRunID:           run.ID,
		Integration:         integration,
		Credential:          credential,
		Arguments:           arguments,
		WindowFrom:          window.From,
		WindowUntil:         window.Until,
	}
	if origin != nil && origin.IntegrationID == integration.ID {
		request.OriginChannel, request.OriginThread = origin.Channel, origin.Thread
	}
	logger := observability.LoggerFor(ctx, slog.Default(), "").With(
		slog.String("org_id", org.String()),
		slog.String("execution_id", execution.ID.String()),
		slog.String("tool_run_id", run.ID.String()),
		slog.String("tool", name),
	)
	logger.DebugContext(ctx, "Tool execution started")
	toolContext, cancel := context.WithTimeout(ctx, integrationToolTimeout)
	result, providerErr := declared.Run(toolContext, request)
	providerFailed := providerErr != nil || toolContext.Err() != nil
	cancel()
	outcome := "succeeded"
	if providerFailed {
		outcome = "failed"
		run.Outcome = investigation.RunFailed
		run.Error = "The Integration Tool did not complete"
	} else {
		run.Outcome = investigation.RunSucceeded
		run.Summary, run.Sources, run.Content = result.Summary, result.Sources, result.Content
		run.Truncated = result.Truncated
		if !result.WindowFrom.IsZero() && !result.WindowUntil.IsZero() {
			run.WindowFrom, run.WindowUntil = result.WindowFrom, result.WindowUntil
			applied := true
			run.WindowApplied = &applied
		}
	}
	investigation.PrepareToolObservation(&run, credential)
	if err := database.CompleteAgentToolRun(ctx, org, execution, run, access); err != nil {
		return nil, errors.New("the Tool observation could not be recorded")
	}
	logger.DebugContext(ctx, "Tool execution finished", slog.String("outcome", outcome))
	if providerFailed {
		return nil, errors.New(run.Error + " (Tool Run " + run.ID.String() + ")")
	}
	return *run.ObservationText, nil
}
