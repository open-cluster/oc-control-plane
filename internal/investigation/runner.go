package investigation

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/seal"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// The bounds an investigation runs under. Every one is named because every one is a
// product decision: they are what "fast, cheap, and not hammering every connected
// system" means in numbers.
const (
	// runTimeout bounds one tool execution.
	runTimeout = 30 * time.Second
	// decideTimeout bounds one reasoner turn; models that think take minutes.
	decideTimeout = 6 * time.Minute
	// investigationTimeout bounds the whole run.
	investigationTimeout = 30 * time.Minute
	// maxConcurrent bounds how many investigations one process runs at once. Each spends
	// model budget and reads connected systems; the per-investigation bounds do not add
	// up to a process-wide one by themselves.
	maxConcurrent = 4
)

// Persisted-text bounds, mirroring the schema's own CHECK constraints (which count
// characters, as these do): a record that says why something failed must never itself
// fail to be recorded for saying it at too much length.
const (
	maxRunErrorLength = 1024
	maxSummaryLength  = 512
)

// bounded cuts text at a rune boundary inside the limit.
func bounded(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}

// Runner executes investigations in the background: offers the sources, reads, records,
// and converses with the model. One per process, so shutdown can drain what is running.
type Runner struct {
	Store   Store
	Catalog integrations.Catalog
	Sealer  seal.Sealer
	// Investigator is the exchange-shaped model boundary the investigation runs against.
	Investigator Investigator
	// Events is where the semantic event stream is written. Nil disables the stream and
	// nothing else: an investigation runs, concludes and records its provenance exactly
	// the same, because the stream is how a run is WATCHED and not what it is.
	Events EventSink
	// MaxToolRuns and MaxTurns are the autonomous loop's ceilings — evaluation-derived
	// tuning, so configuration rather than constants; zero means the defaults.
	MaxToolRuns int
	MaxTurns    int
	Logger      *slog.Logger
	// SpendCeilingMicroCents is the hard spend ceiling per investigation. A reached
	// ceiling forces the concluding turn and is recorded as stopped_by — an honest
	// partial investigation, never a failure and never a diagnosis it did not reach.
	// Zero means no ceiling, which only a test should mean.
	SpendCeilingMicroCents int64

	running sync.WaitGroup
	mu      sync.Mutex
	active  int
	// base is the process-lifetime context every investigation runs under; cancelling it
	// fails what is still running rather than orphaning it.
	base   context.Context
	cancel context.CancelFunc
	once   sync.Once
}

// AtCapacity reports whether starting another investigation now would exceed the
// process's concurrency bound. The handler consults it BEFORE creating the record, so a
// refusal leaves nothing behind; the small race two concurrent opens can win is bounded
// by every investigation's own budgets.
func (r *Runner) AtCapacity() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active >= maxConcurrent
}

// Start launches one investigation in the background. The record already exists and is
// running; this fills its provenance and ends it.
func (r *Runner) Start(organization tenancy.Organization, opened Investigation) {
	r.once.Do(func() {
		r.base, r.cancel = context.WithCancel(context.Background())
	})
	r.mu.Lock()
	r.active++
	r.mu.Unlock()
	r.running.Add(1)
	go func() {
		defer func() {
			r.mu.Lock()
			r.active--
			r.mu.Unlock()
			r.running.Done()
		}()
		ctx, done := context.WithTimeout(r.base, investigationTimeout)
		defer done()
		r.run(ctx, organization, opened)
	}()
}

// Drain stops taking the process down with investigations mid-flight: what is running is
// cancelled — each fails with the reason recorded — and waited for.
func (r *Runner) Drain() {
	r.once.Do(func() {
		r.base, r.cancel = context.WithCancel(context.Background())
	})
	r.cancel()
	r.running.Wait()
}

// droppedRun records a call that was proposed and not executed, so the drop is on the
// record rather than silent.
func droppedRun(opened Investigation, call ToolCall, ordinal int, reason string) ToolRun {
	now := time.Now().UTC()
	return ToolRun{
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
func (r *Runner) execute(
	ctx context.Context, opened Investigation, selected []selection,
	credentials *credentialCache, call ToolCall, ordinal int,
) ToolRun {
	run := ToolRun{
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
		return run
	}
	run.IntegrationID = source.integration.ID
	run.Capability = tool.Capability

	credential, err := credentials.open(ctx, source.integration)
	if err != nil {
		run.Outcome = RunFailed
		run.Error = "the integration's credential could not be opened"
		run.FinishedAt = time.Now().UTC()
		return run
	}

	runCtx, done := context.WithTimeout(ctx, runTimeout)
	defer done()
	result, err := tool.Run(runCtx, integrations.ToolRequest{
		Integration: source.integration,
		Credential:  credential,
		Arguments:   call.Arguments,
		WindowFrom:  opened.WindowFrom,
		WindowUntil: opened.WindowUntil,
	})
	run.FinishedAt = time.Now().UTC()
	if err != nil {
		run.Outcome = RunFailed
		run.Error = bounded(err.Error(), maxRunErrorLength)
		return run
	}
	run.Outcome = RunSucceeded
	run.Truncated = result.Truncated
	run.Summary = bounded(result.Summary, maxSummaryLength)
	run.Sources = result.Sources
	run.Content = result.Content
	return run
}

// fail ends the investigation with the reason, writing inside a detached window so a
// cancelled run can still say why it stopped. The terminal event is written in the same
// window and for the same reason: a reader left watching a spinner forever is exactly the
// failure this is here to prevent.
func (r *Runner) fail(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID,
	events *stream, reason string, spend Spend,
) {
	writeCtx, done := writeWindow(ctx)
	defer done()
	reason = bounded(reason, maxRunErrorLength)
	if err := r.Store.FailInvestigation(writeCtx, organization, id, reason, spend); err != nil {
		r.Logger.Error("a failed investigation could not be recorded as failed",
			slog.String("investigation_id", id.String()),
			slog.String("error", err.Error()))
	}
	r.announce(writeCtx, events, EventFailed, failedPayload(reason))
}

// announce writes one event and swallows the failure into a log line.
//
// An event that could not be written must not end an investigation. The record is the
// investigation and its provenance; the stream is a view of it being produced, and trading
// a completed diagnosis for a missing progress line would be the wrong way round. A write
// that fails is visible as a gap in the sequence, which is what the log line explains.
func (r *Runner) announce(
	ctx context.Context, events *stream, eventType EventType, payload map[string]any,
) {
	if err := events.emit(ctx, eventType, payload); err != nil {
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
	case StoppedBySpend:
		return "Stopping the reads: the investigation reached its spend ceiling"
	case StoppedByToolRuns:
		return "Stopping the reads: the investigation used its read budget"
	case StoppedByReasonerTurns:
		return "Stopping the reads: the investigation used its turn budget"
	case StoppedByWallClock:
		return "Stopping the reads: the investigation is nearly out of time"
	case StoppedByStagnation:
		return "Stopping the reads: the last few produced no new evidence"
	case StoppedByContext:
		return "Stopping the reads: this turn has filled the model's working context"
	default:
		return "Stopping the reads"
	}
}

// writeWindow keeps a final write possible after the run's own context ended, bounded so
// shutdown cannot hang on it.
func writeWindow(ctx context.Context) (context.Context, context.CancelFunc) {
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
func checkCitations(findings []Finding, runs int) string {
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
		c.fail[integration.ID] = err
		return "", err
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
