package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/seal"
	"github.com/open-cluster/oc-control-plane/internal/telemetry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

func decodeToolCalls(calls []CompletionCall) []toolCall {
	translated := make([]toolCall, 0, len(calls))
	for _, call := range calls {
		var arguments map[string]any
		translatedCall := toolCall{
			ID:   call.ID,
			Tool: call.Name,
		}

		if err := json.Unmarshal(call.Arguments, &arguments); err != nil || arguments == nil {
			translatedCall.Error = "Tool arguments must be a valid JSON object"
		} else if call.Name == historyToolName {
			translatedCall.Arguments = arguments
		} else {
			if selected, present := arguments["integrationId"]; present {
				id, ok := selected.(string)
				if !ok || id == "" {
					translatedCall.Error = "Tool integrationId must be a nonempty string when supplied"
				} else {
					translatedCall.IntegrationID = id
				}
			}
			input, ok := arguments["input"].(map[string]any)
			if !ok || input == nil {
				translatedCall.Error = "Tool input must be a JSON object"
			} else {
				translatedCall.Arguments = input
			}
			if purpose, present := arguments["purpose"]; present {
				if text, ok := purpose.(string); ok {
					translatedCall.Purpose = text
				} else {
					translatedCall.Error = "Tool purpose must be a string when supplied"
				}
			}
			for field := range arguments {
				if field != "purpose" && field != "input" && field != "integrationId" {
					translatedCall.Error = "Tool arguments accept only input, optional purpose and source selection"
				}
			}
		}
		translated = append(translated, translatedCall)
	}
	return translated
}

func offeredTools(
	definition integrations.Definition,
	candidate integrations.Integration) []integrations.Tool {
	return integrations.SupportedTools(definition, candidate)
}

func sortSourcesByName(sources []offeredSource) {
	sort.SliceStable(sources, func(i, j int) bool {
		return sources[i].Integration.Name < sources[j].Integration.Name
	})
}

const (
	runTimeout        = 30 * time.Second
	maxRunErrorLength = 1024
)

var errCredentialAudit = errors.New("credential access could not be audited")

func boundText(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}

func droppedRun(opened investigation.Investigation, call toolCall, ordinal int, reason string) investigation.ToolRun {
	now := time.Now().UTC()
	return investigation.ToolRun{
		Ordinal:     ordinal,
		Tool:        call.Tool,
		Arguments:   call.Arguments,
		WindowFrom:  opened.WindowFrom,
		WindowUntil: opened.WindowUntil,
		Outcome:     investigation.RunFailed,
		Summary:     reason,
		Error:       reason,
		StartedAt:   now,
		FinishedAt:  now,
	}
}

func (r *Agent) execute(
	ctx context.Context, organization uuid.UUID, opened investigation.Investigation, selected []offeredSource,
	credentials *credentialCache, origin *investigation.ConversationOrigin,
	call toolCall, ordinal int,
) (investigation.ToolRun, error) {
	run := investigation.ToolRun{
		ID:          uuid.New(),
		Ordinal:     ordinal,
		Tool:        call.Tool,
		Arguments:   call.Arguments,
		WindowFrom:  opened.WindowFrom,
		WindowUntil: opened.WindowUntil,
		StartedAt:   time.Now().UTC(),
	}

	source, tool, offered := toolNamed(selected, call)
	if !offered {
		run.Outcome = investigation.RunFailed
		run.Error = "not one of the tools the selected sources offer"
		run.FinishedAt = time.Now().UTC()
		return run, nil
	}
	run.IntegrationID = source.Integration.ID
	integration, err := r.Store.Integration(ctx, organization, source.Integration.ID)
	if err != nil {
		run.Outcome = investigation.RunFailed
		run.Error = "the integration is no longer available"
		run.FinishedAt = time.Now().UTC()
		return run, nil
	}
	definition, known := r.Catalog.Lookup(integration.Provider)
	eligible := false
	if known {
		for _, availableTool := range integrations.SupportedTools(definition, integration) {
			if availableTool.Name == tool.Name {
				eligible = true
				tool = availableTool
				break
			}
		}
	}
	if !eligible {
		run.Outcome = investigation.RunFailed
		run.Error = "the integration no longer authorizes this tool"
		run.FinishedAt = time.Now().UTC()
		return run, nil
	}
	source.Integration = integration

	credential, err := credentials.open(ctx, source.Integration)
	if err != nil {
		run.Outcome = investigation.RunFailed
		run.Error = "the integration's credential could not be opened"
		run.FinishedAt = time.Now().UTC()
		if errors.Is(err, errCredentialAudit) {
			return run, err
		}
		return run, nil
	}

	runCtx, done := context.WithTimeout(ctx, runTimeout)
	defer done()
	request := integrations.ToolRequest{
		InvestigationID: opened.ID,
		Integration:     source.Integration,
		Credential:      credential,
		Arguments:       call.Arguments,
		WindowFrom:      opened.WindowFrom,
		WindowUntil:     opened.WindowUntil,
	}
	if origin != nil && source.Integration.ID == origin.IntegrationID {
		request.OriginChannel = origin.Channel
		request.OriginThread = origin.Thread
	}

	// The tool Run implementation execution ----->
	runCtx, span := otel.Tracer(meterName).Start(runCtx, "investigation.tool", trace.WithAttributes(
		attribute.String("oc.tool.name", tool.Name),
		attribute.String("oc.integration.id", run.IntegrationID.String()),
		attribute.Int("oc.tool.ordinal", ordinal)))
	defer span.End()
	result, err := tool.Run(runCtx, request)
	run.FinishedAt = time.Now().UTC()
	level, outcome := slog.LevelDebug, "succeeded"
	if err != nil {
		outcome = "failed"
		if !errors.Is(err, context.Canceled) && runCtx.Err() != context.Canceled {
			span.SetStatus(codes.Error, "tool execution failed")
			var networkError net.Error
			if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &networkError) || runCtx.Err() == context.DeadlineExceeded {
				level = slog.LevelWarn
			}
		}
	}
	span.SetAttributes(attribute.String("oc.tool.outcome", outcome))
	observability.LoggerFor(runCtx, r.Logger, "").Log(
		runCtx, level, "Tool execution finished",
		slog.String("integration_id", run.IntegrationID.String()),
		slog.String("tool", tool.Name), slog.Int("ordinal", ordinal),
		slog.String("outcome", outcome),
		slog.Float64("duration_ms", float64(run.FinishedAt.Sub(run.StartedAt))/float64(time.Millisecond)))
	if err != nil {
		run.Outcome = investigation.RunFailed
		run.Error = err.Error()
		return run, nil
	}
	run.Outcome = investigation.RunSucceeded
	if !result.WindowFrom.IsZero() && !result.WindowUntil.IsZero() {
		run.WindowFrom, run.WindowUntil = result.WindowFrom, result.WindowUntil
		applied := true
		run.WindowApplied = &applied
	}
	run.Truncated = result.Truncated
	run.Summary = result.Summary
	run.Sources = result.Sources
	run.Content = result.Content
	return run, nil
}

func (r *Agent) persistFailure(
	ctx context.Context, organization uuid.UUID, id uuid.UUID, token uuid.UUID,
	reason string, usage investigation.Usage,
) error {
	writeCtx, done := terminalWriteWindow(ctx)
	defer done()
	reason = boundText(reason, maxRunErrorLength)
	if err := r.Store.FailInvestigation(writeCtx, organization, id, token, reason, usage); err != nil {
		return fmt.Errorf("recording investigation failure: %w", err)
	}
	return nil
}

func (r *Agent) announce(
	ctx context.Context, events *investigation.EventStream, payload investigation.EventPayload,
) {
	if err := events.Emit(ctx, payload); err != nil {
		observability.LoggerFor(ctx, r.Logger, "").WarnContext(ctx, "Investigation event could not be written",
			slog.String("event", payload.EventType().String()),
			slog.String("error", "event could not be recorded"))
	}
}

const maxRunContentBytes = investigation.MaxToolContentBytes

func ceilingProgress(stoppedBy string) string {
	switch stoppedBy {
	case investigation.StoppedByToolRuns:
		return "Stopping the reads: the investigation used its read budget"
	case investigation.StoppedByReasonerTurns:
		return "Stopping the reads: the investigation used its turn budget"
	case investigation.StoppedByWallClock:
		return "Stopping the reads: the investigation is nearly out of time"
	case investigation.StoppedByContext:
		return "Stopping the reads: this turn has filled the model's working context"
	default:
		return "Stopping the reads"
	}
}

func terminalWriteWindow(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
}

func reasonerFailure(err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "the investigation was stopped before the reasoner answered"
	}
	return "the reasoning step could not run: " + firstLine(err.Error())
}

func firstLine(text string) string {
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		return text[:index]
	}
	return text
}

func toolNamed(selected []offeredSource, call toolCall) (offeredSource, integrations.Tool, bool) {
	var match offeredSource
	var matched integrations.Tool
	found := 0
	for _, source := range selected {
		if call.IntegrationID != "" && source.Integration.ID.String() != call.IntegrationID {
			continue
		}
		for _, tool := range source.Tools {
			if tool.Name == call.Tool {
				match, matched = source, tool
				found++
			}
		}
	}
	if found == 1 {
		return match, matched, true
	}
	return offeredSource{}, integrations.Tool{}, false
}

type credentialCache struct {
	sealer           seal.Sealer
	record           func(ctx context.Context, id uuid.UUID) error
	opened           map[uuid.UUID]string
	fail             map[uuid.UUID]error
	sealed           map[uuid.UUID]string
	knownCredentials []string
}

func newCredentialCache(
	sealer seal.Sealer, record func(ctx context.Context, id uuid.UUID) error,
) *credentialCache {
	return &credentialCache{
		sealer: sealer,
		record: record,
		opened: map[uuid.UUID]string{},
		fail:   map[uuid.UUID]error{},
		sealed: map[uuid.UUID]string{},
	}
}

func (c *credentialCache) open(
	ctx context.Context, integration integrations.Integration,
) (string, error) {
	if c.sealed[integration.ID] != string(integration.CredentialSealed) {
		delete(c.opened, integration.ID)
		delete(c.fail, integration.ID)
		c.sealed[integration.ID] = string(integration.CredentialSealed)
	}
	if credential, held := c.opened[integration.ID]; held {
		return credential, nil
	}
	if err, failed := c.fail[integration.ID]; failed {
		return "", err
	}
	if len(integration.CredentialSealed) == 0 {
		c.opened[integration.ID] = ""
		return "", nil
	}
	if err := c.record(ctx, integration.ID); err != nil {
		auditErr := fmt.Errorf("%w: %v", errCredentialAudit, err)
		c.fail[integration.ID] = auditErr
		return "", auditErr
	}
	credential, err := c.sealer.Open(integration.CredentialSealed,
		integrations.CredentialBinding(integration.ID))
	if err != nil {
		c.fail[integration.ID] = err
		return "", err
	}
	c.opened[integration.ID] = credential
	c.knownCredentials = append(c.knownCredentials, credential)
	return credential, nil
}
