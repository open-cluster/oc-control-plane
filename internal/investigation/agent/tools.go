package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/seal"
)

type selection struct {
	integration integrations.Integration
	tools       []integrations.Tool
}

func agentCalls(calls []CompletionCall) []toolCall {
	translated := make([]toolCall, 0, len(calls))
	for _, call := range calls {
		arguments := map[string]any{}
		if len(call.Arguments) > 0 {
			_ = json.Unmarshal(call.Arguments, &arguments)
		}
		translatedCall := toolCall{ID: call.ID, Tool: call.Name}
		if call.Name == historyToolName {
			translatedCall.Arguments = arguments
		} else {
			translatedCall.Purpose, _ = arguments["purpose"].(string)
			translatedCall.Arguments, _ = arguments["input"].(map[string]any)
			if translatedCall.Arguments == nil {
				translatedCall.Arguments = map[string]any{}
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
	maxSummaryLength  = 512
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
		Error:       reason,
		StartedAt:   now,
		FinishedAt:  now,
	}
}

func (r *Agent) execute(
	ctx context.Context, opened investigation.Investigation, selected []selection,
	credentials *credentialCache, origin *investigation.ConversationOrigin,
	call toolCall, ordinal int,
) (investigation.ToolRun, error) {
	run := investigation.ToolRun{
		Ordinal:     ordinal,
		Tool:        call.Tool,
		Arguments:   call.Arguments,
		WindowFrom:  opened.WindowFrom,
		WindowUntil: opened.WindowUntil,
		StartedAt:   time.Now().UTC(),
	}

	source, tool, offered := toolNamed(selected, call.Tool)
	if !offered {
		run.Outcome = investigation.RunFailed
		run.Error = "not one of the tools the selected sources offer"
		run.FinishedAt = time.Now().UTC()
		return run, nil
	}
	run.IntegrationID = source.integration.ID

	credential, err := credentials.open(ctx, source.integration)
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
		Integration:     source.integration,
		Credential:      credential,
		Arguments:       call.Arguments,
		WindowFrom:      opened.WindowFrom,
		WindowUntil:     opened.WindowUntil,
	}
	if origin != nil && source.integration.ID == origin.IntegrationID {
		request.OriginChannel = origin.Channel
		request.OriginThread = origin.Thread
	}
	result, err := tool.Run(runCtx, request)
	run.FinishedAt = time.Now().UTC()
	if err != nil {
		run.Outcome = investigation.RunFailed
		run.Error = boundText(err.Error(), maxRunErrorLength)
		return run, nil
	}
	run.Outcome = investigation.RunSucceeded
	if !result.WindowFrom.IsZero() && !result.WindowUntil.IsZero() {
		run.WindowFrom, run.WindowUntil = result.WindowFrom, result.WindowUntil
		run.WindowApplied = true
	}
	run.Truncated = result.Truncated
	run.Summary = boundText(result.Summary, maxSummaryLength)
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
		r.Logger.Warn("an investigation event could not be written",
			slog.String("event", payload.EventType().String()),
			slog.String("error", err.Error()))
	}
}

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

func checkCitations(findings []investigation.Finding, runs int) string {
	for _, finding := range findings {
		if len(finding.RunRefs) == 0 && len(finding.EvidenceRefs) == 0 {
			return "the reasoner stated a finding citing no read at all"
		}
		if len(finding.RunRefs) == 0 {
			continue
		}
		cited := append([]int(nil), finding.RunRefs...)
		sort.Ints(cited)
		if cited[0] < 1 || cited[len(cited)-1] > runs {
			return "the reasoner cited a read that never ran"
		}
	}
	return ""
}

func toolNamed(selected []selection, name string) (selection, integrations.Tool, bool) {
	for _, source := range selected {
		for _, tool := range source.tools {
			if tool.Name == name {
				return source, tool, true
			}
		}
	}
	var match selection
	var matched integrations.Tool
	found := 0
	for _, source := range selected {
		for _, tool := range source.tools {
			if base, _, bound := strings.Cut(tool.Name, "__"); bound && base == name {
				match, matched = source, tool
				found++
			}
		}
	}
	if found == 1 {
		return match, matched, true
	}
	return selection{}, integrations.Tool{}, false
}

type credentialCache struct {
	sealer seal.Sealer
	record func(ctx context.Context, id uuid.UUID) error
	opened map[uuid.UUID]string
	fail   map[uuid.UUID]error
}

func newCredentialCache(
	sealer seal.Sealer, record func(ctx context.Context, id uuid.UUID) error,
) *credentialCache {
	return &credentialCache{
		sealer: sealer,
		record: record,
		opened: map[uuid.UUID]string{},
		fail:   map[uuid.UUID]error{},
	}
}

func (c *credentialCache) open(
	ctx context.Context, integration integrations.Integration,
) (string, error) {
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
	return credential, nil
}

const answerCutMark = "… [truncated: the full account is in the findings]"

func boundedSummary(text string) string {
	runes := []rune(text)
	if len(runes) <= investigation.MaxSummaryLength {
		return text
	}
	mark := []rune(answerCutMark)
	return string(runes[:investigation.MaxSummaryLength-len(mark)]) + answerCutMark
}
