package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/secrets"
)

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

// droppedRun records a call that was proposed and not executed, so the drop is on the
// record rather than silent.
func droppedRun(opened investigation.Investigation, call investigation.ToolCall, ordinal int, reason string) investigation.ToolRun {
	now := time.Now().UTC()
	return investigation.ToolRun{
		Ordinal:     ordinal,
		Tool:        call.Tool,
		Arguments:   call.Arguments,
		WindowFrom:  opened.WindowFrom,
		WindowUntil: opened.WindowUntil,
		Outcome:     RunFailed,
		Error:       reason,
		StartedAt:   now,
		FinishedAt:  now,
	}
}

// execute performs one proposed read and reports it as a run, success or failure alike:
// a read that failed is provenance too, and often the provenance that matters.
func (r *Agent) execute(
	ctx context.Context, opened investigation.Investigation, selected []selection,
	credentials *credentialCache, brief *Brief, call investigation.ToolCall, ordinal int,
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
		run.Outcome = RunFailed
		run.Error = "not one of the tools the selected sources offer"
		run.FinishedAt = time.Now().UTC()
		return run, nil
	}
	run.IntegrationID = source.integration.ID

	credential, err := credentials.open(ctx, source.integration)
	if err != nil {
		run.Outcome = RunFailed
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
	if brief != nil && source.integration.ID.String() == brief.OriginIntegrationID {
		request.OriginChannel = brief.OriginChannel
		request.OriginThread = brief.OriginThread
	}
	result, err := tool.Run(runCtx, request)
	run.FinishedAt = time.Now().UTC()
	if err != nil {
		run.Outcome = RunFailed
		run.Error = boundText(err.Error(), maxRunErrorLength)
		return run, nil
	}
	run.Outcome = RunSucceeded
	// A windowed tool reports the window it ACTUALLY read, which is not what was asked
	// for whenever the clamp narrowed it — including a call phrased with no window at
	// all. A tool that reads no window leaves the investigation's own in place, so the
	// record always names the bound in force.
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

// fail ends the investigation with the reason, writing inside a detached window so a
// cancelled run can still say why it stopped. The terminal event is written in the same
// window and for the same reason: a reader left watching a spinner forever is exactly the
// failure this is here to prevent.
func (r *Agent) fail(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID,
	events *investigation.EventStream, reason string, spend investigation.Spend,
) error {
	writeCtx, done := terminalWriteWindow(ctx)
	defer done()
	reason = boundText(reason, maxRunErrorLength)
	if err := r.Store.FailInvestigation(writeCtx, organization, id, reason, spend); err != nil {
		return fmt.Errorf("recording investigation failure: %w", err)
	}
	r.announce(writeCtx, events, investigation.EventFailed, investigation.FailedPayload(reason))
	return nil
}

// announce writes one event and swallows the failure into a log line.
//
// An event that could not be written must not end an investigation. The record is the
// investigation and its provenance; the stream is a view of it being produced, and trading
// a completed diagnosis for a missing progress line would be the wrong way round. A write
// that fails is visible as a gap in the sequence, which is what the log line explains.
func (r *Agent) announce(
	ctx context.Context, events *investigation.EventStream, eventType investigation.EventType, payload map[string]any,
) {
	if err := events.Emit(ctx, eventType, payload); err != nil {
		r.Logger.Warn("an investigation event could not be written",
			slog.String("event", eventType.String()),
			slog.String("error", err.Error()))
	}
}

// ceilingProgress is what a person watching reads when the reads end. Composed HERE from
// the ceiling that fired — the model is never asked to narrate, so there is no reasoning to
// leak and no prompt surface to review.
func ceilingProgress(stoppedBy string) string {
	switch stoppedBy {
	case investigation.StoppedBySpend:
		return "Stopping the reads: the investigation reached its spend ceiling"
	case investigation.StoppedByToolRuns:
		return "Stopping the reads: the investigation used its read budget"
	case investigation.StoppedByReasonerTurns:
		return "Stopping the reads: the investigation used its turn budget"
	case investigation.StoppedByWallClock:
		return "Stopping the reads: the investigation is nearly out of time"
	case investigation.StoppedByStagnation:
		return "Stopping the reads: the last few produced no new evidence"
	case investigation.StoppedByContext:
		return "Stopping the reads: this turn has filled the model's working context"
	default:
		return "Stopping the reads"
	}
}

// writeWindow keeps a final write possible after the run's own context ended, bounded so
// shutdown cannot hang on it.
func terminalWriteWindow(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
}

// reasonerFailure says why the reasoning step could not run, in words safe for the
// record: the named outcome, never the provider's own prose.
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

// checkCitations refuses findings citing runs that never happened. The reasoning
// infrastructure already validates this at decode; checking again here means a test
// double cannot accidentally store an untraceable finding either.
func checkCitations(findings []investigation.Finding, runs int) string {
	for _, finding := range findings {
		if len(finding.Sources) == 0 {
			return "the reasoner stated a finding citing no read at all"
		}
		cited := append([]int(nil), finding.Sources...)
		sort.Ints(cited)
		if cited[0] < 1 || cited[len(cited)-1] > runs {
			return "the reasoner cited a read that never ran"
		}
	}
	return ""
}

// toolNamed resolves a proposed tool among the selected sources. Names are globally
// unique — every provider prefixes its own — so the first match is the match.
func toolNamed(selected []selection, name string) (selection, integrations.Tool, bool) {
	for _, source := range selected {
		for _, tool := range source.tools {
			if tool.Name == name {
				return source, tool, true
			}
		}
	}
	// Compatibility for a run opened before a second same-type Integration joined
	// the offer: the old provider Tool name still resolves deterministically, while the
	// Integration-bound names make every colliding source explicitly reachable.
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

// credentialCache opens each integration's credential once per investigation, not once
// per call — which also means one audit record per integration per investigation, not
// one per read. Not safe for concurrent use; one investigation runs its calls in
// sequence.
type credentialCache struct {
	sealer seal.Sealer
	// record writes the unseal's audit event. It runs BEFORE the credential is opened:
	// a use that cannot be recorded does not happen.
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

// answerCutMark is what a truncated answer ends with. An answer that stops mid-sentence
// with no mark is indistinguishable from one that finished, and an operator acting on
// half a sentence is the failure this exists to prevent.
const answerCutMark = "… [truncated: the full account is in the findings]"

// boundedSummary holds the operator summary inside its bound and says so when it cuts. The
// mark is charged against the bound rather than appended past it, so the result is always
// within the ceiling the record is written under.
func boundedSummary(text string) string {
	runes := []rune(text)
	if len(runes) <= investigation.MaxSummaryLength {
		return text
	}
	mark := []rune(answerCutMark)
	return string(runes[:investigation.MaxSummaryLength-len(mark)]) + answerCutMark
}
