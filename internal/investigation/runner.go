package investigation

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
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
	// maxRounds is how many decisions the reasoner gets; the last one must conclude.
	maxRounds = 4
	// maxCallsPerRound bounds one decision's reads.
	maxCallsPerRound = 4
	// maxRuns bounds the whole investigation's reads.
	maxRuns = 12
	// runTimeout bounds one tool execution.
	runTimeout = 30 * time.Second
	// decideTimeout bounds one reasoner decision; models that think take minutes.
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
	maxReasonLength   = 512
)

// bounded cuts text at a rune boundary inside the limit.
func bounded(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}

// Runner executes investigations in the background: routes, reads, records, and asks
// the reasoner to decide. One per process, so shutdown can drain what is running.
type Runner struct {
	Store    Store
	Catalog  integrations.Catalog
	Sealer   seal.Sealer
	Reasoner Reasoner
	Logger   *slog.Logger

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

// run is one whole investigation: route, then a bounded loop of reasoner decisions and
// tool executions, then the conclusion or the failure — every step recorded as it
// happens, so an investigation that dies mid-flight still shows how far it got.
func (r *Runner) run(
	ctx context.Context, organization tenancy.Organization, opened Investigation,
) {
	spend := Spend{}

	candidates, err := r.Store.InvestigationCandidates(ctx, organization)
	if err != nil {
		r.fail(ctx, organization, opened.ID, "the connected sources could not be read", spend)
		return
	}
	selected, remainder := route(r.Catalog, candidates, r.routingTerms(ctx, organization, opened))
	for rank, source := range selected {
		if err := r.recordSource(ctx, organization, opened.ID, source, rank+1); err != nil {
			r.fail(ctx, organization, opened.ID, "the routing could not be recorded", spend)
			return
		}
	}

	credentials := newCredentialCache(r.Sealer)
	var runs []ToolRun
	expanded := false

	for round := 1; ; round++ {
		mustConclude := round >= maxRounds || len(runs) >= maxRuns || len(selected) == 0

		decision, decideErr := r.decide(ctx, opened, selected, runs, mustConclude)
		spend = spend.Add(decision.Spend)
		if decideErr != nil {
			r.fail(ctx, organization, opened.ID, reasonerFailure(decideErr), spend)
			return
		}

		if mustConclude || len(decision.Calls) == 0 {
			if citation := checkCitations(decision.Findings, len(runs)); citation != "" {
				r.fail(ctx, organization, opened.ID, citation, spend)
				return
			}
			writeCtx, done := writeWindow(ctx)
			defer done()
			if err := r.Store.ConcludeInvestigation(
				writeCtx, organization, opened.ID, decision.Findings, spend); err != nil {
				r.Logger.Error("an investigation's conclusion could not be recorded",
					slog.String("investigation_id", opened.ID.String()),
					slog.String("error", err.Error()))
			}
			return
		}

		calls := decision.Calls
		if len(calls) > maxCallsPerRound {
			calls = calls[:maxCallsPerRound]
		}
		for _, call := range calls {
			if len(runs) >= maxRuns {
				break
			}
			run := r.execute(ctx, opened, selected, credentials, call, len(runs)+1)
			runs = append(runs, run)
			if err := r.Store.RecordToolRun(ctx, organization, opened.ID, run); err != nil {
				r.fail(ctx, organization, opened.ID, "a tool run could not be recorded", spend)
				return
			}
		}

		// The one expansion, and only when the whole first selection produced nothing:
		// widening a search that is finding things is how every connected system gets
		// hammered, which this router exists to prevent.
		if !expanded && nothingFound(runs) && len(remainder) > 0 {
			expanded = true
			more := remainder
			if len(more) > expansionSources {
				more = more[:expansionSources]
			}
			for offset, source := range more {
				source.reason = "expanded after the first selection returned nothing: " +
					source.reason
				if err := r.recordSource(ctx, organization, opened.ID, source,
					len(selected)+offset+1); err != nil {
					r.fail(ctx, organization, opened.ID, "the routing could not be recorded", spend)
					return
				}
				selected = append(selected, source)
			}
			remainder = remainder[len(more):]
		}
	}
}

// routingTerms is what the router matches candidates against: the subject, the
// operator's question, and the triggering alert's own labels — a namespace or service
// label is exactly the term that picks the right channel and repository.
func (r *Runner) routingTerms(
	ctx context.Context, organization tenancy.Organization, opened Investigation,
) []string {
	parts := []string{opened.Subject, opened.Question}
	if opened.EpisodeID != uuid.Nil {
		// Best-effort: an unreadable trigger narrows the terms, never fails the run.
		if trigger, err := r.Store.TriggerEpisode(ctx, organization, opened.EpisodeID); err == nil {
			for key, value := range trigger.Labels {
				parts = append(parts, key, value)
			}
		}
	}
	return subjectTerms(parts...)
}

// decide asks the reasoner for the next move, under its own deadline.
func (r *Runner) decide(
	ctx context.Context, opened Investigation, selected []selection, runs []ToolRun,
	mustConclude bool,
) (Decision, error) {
	sources := make([]BriefSource, 0, len(selected))
	for _, source := range selected {
		sources = append(sources, BriefSource{
			Integration: source.integration,
			Tools:       source.tools,
		})
	}
	brief := Brief{
		Subject:      opened.Subject,
		Question:     opened.Question,
		WindowFrom:   opened.WindowFrom,
		WindowUntil:  opened.WindowUntil,
		Sources:      sources,
		Runs:         runs,
		MustConclude: mustConclude,
	}

	decideCtx, done := context.WithTimeout(ctx, decideTimeout)
	defer done()
	return r.Reasoner.Decide(decideCtx, brief)
}

// execute performs one proposed read and reports it as a run, success or failure alike:
// a read that failed is provenance too, and often the provenance that matters.
func (r *Runner) execute(
	ctx context.Context, opened Investigation, selected []selection,
	credentials *credentialCache, call ToolCall, ordinal int,
) ToolRun {
	run := ToolRun{
		ID:          uuid.New(),
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

	credential, err := credentials.open(source.integration)
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

func (r *Runner) recordSource(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID,
	source selection, rank int,
) error {
	return r.Store.RecordSource(ctx, organization, id, Source{
		IntegrationID: source.integration.ID,
		Rank:          rank,
		Reason:        bounded(source.reason, maxReasonLength),
		SelectedAt:    time.Now().UTC(),
	})
}

// fail ends the investigation with the reason, writing inside a detached window so a
// cancelled run can still say why it stopped.
func (r *Runner) fail(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID,
	reason string, spend Spend,
) {
	writeCtx, done := writeWindow(ctx)
	defer done()
	reason = bounded(reason, maxRunErrorLength)
	if err := r.Store.FailInvestigation(writeCtx, organization, id, reason, spend); err != nil {
		r.Logger.Error("a failed investigation could not be recorded as failed",
			slog.String("investigation_id", id.String()),
			slog.String("error", err.Error()))
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

// nothingFound reports whether every run so far failed or came back empty, which is what
// justifies the one expansion.
func nothingFound(runs []ToolRun) bool {
	for _, run := range runs {
		if run.Outcome == RunSucceeded && !emptyContent(run.Content) {
			return false
		}
	}
	return true
}

// emptyContent reports whether a run's answer holds nothing. Tool contents are slices of
// items or single records; reflection here keeps the judgement out of every provider.
func emptyContent(content any) bool {
	if content == nil {
		return true
	}
	value := reflect.ValueOf(content)
	switch value.Kind() {
	case reflect.Slice, reflect.Map:
		return value.Len() == 0
	default:
		return false
	}
}

// credentialCache opens each integration's credential once per investigation, not once
// per call. Not safe for concurrent use; one investigation runs its calls in sequence.
type credentialCache struct {
	sealer seal.Sealer
	opened map[uuid.UUID]string
	fail   map[uuid.UUID]error
}

func newCredentialCache(sealer seal.Sealer) *credentialCache {
	return &credentialCache{
		sealer: sealer,
		opened: map[uuid.UUID]string{},
		fail:   map[uuid.UUID]error{},
	}
}

func (c *credentialCache) open(integration integrations.Integration) (string, error) {
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
	credential, err := c.sealer.Open(integration.CredentialSealed)
	if err != nil {
		c.fail[integration.ID] = err
		return "", err
	}
	c.opened[integration.ID] = credential
	return credential, nil
}
