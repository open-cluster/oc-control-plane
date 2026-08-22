package investigation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
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
	// Leases is how work is claimed, kept and recovered. Nil means this process runs
	// investigations without claiming them — which is what a unit test wants, and what a
	// deployment must never have, because an unclaimed investigation whose worker dies is
	// the record that says `running` forever.
	Leases Leases
	// Worker identifies this process among the live ones. Empty means the leases are
	// taken under a name derived at first use.
	Worker string
	// OrgConcurrent is the most investigations one organization may have executing at
	// once, so one tenant cannot consume the whole deployment. Zero means the default.
	OrgConcurrent int
	// WindowLead is how far back a drained conversation turn's window reaches. It matches
	// the operator surface's own, because a follow-up asks about the same period the
	// question before it did.
	WindowLead time.Duration
	// ContextCeiling is how many tokens a turn may carry in total — the conversation it
	// was handed AND the reads it has made — before its conclusion is forced.
	//
	// It is a SECOND number, deliberately above ContextBudget, and the gap between them
	// is the room a compaction buys. They were one number, and that made compaction
	// pointless for the turn doing it: the ceiling counts the tool catalogue too, which is
	// never zero, so it was always crossed first and every compacting turn arrived already
	// told to conclude. Zero means no ceiling, which only a test should mean.
	ContextCeiling int
	// ContextBudget is how many tokens of conversation context a turn may carry before
	// older turns are compacted. Computed by the composition root from the model and the
	// deployment's threshold, because THIS package must never learn what a vendor is.
	// Zero means no budget, which only a test should mean.
	ContextBudget int
	// ModelName is recorded beside a summary, so a compaction that happened under one
	// model's budget is legible after a deployment moves to another. It is a label and
	// nothing here behaves differently for it.
	ModelName string
	// MaxToolRuns and MaxTurns are the autonomous loop's ceilings — evaluation-derived
	// tuning, so configuration rather than constants; zero means the defaults.
	MaxToolRuns int
	MaxTurns    int
	Logger      *slog.Logger
	// Telemetry measures the runtime: how long work waits, how long a reader waits, how
	// long a run takes, and how often memory is consolidated or a worker died. Nil
	// measures nothing and breaks nothing.
	Telemetry *Telemetry
	// HeartbeatEvery is how often a held lease is renewed. Zero means the default, which
	// is what a deployment wants; a test sets its own so it can wait on a heartbeat
	// instead of on two real minutes. It belongs to the runner rather than the package so
	// that two runners in one process cannot reach into each other's timing.
	HeartbeatEvery time.Duration
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
	base     context.Context
	cancel   context.CancelFunc
	once     sync.Once
	identity sync.Once
	worker   string
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
//
// It takes the LEASE first, in this process, rather than leaving the record for the claim
// loop to find. Two reasons: the single-shot path answers a request that is waiting, and a
// poll interval of latency on it would be latency added for nothing; and an investigation
// with no lease at all is invisible to the sweeper, which is exactly the record that says
// `running` forever after a crash.
func (r *Runner) Start(organization tenancy.Organization, opened Investigation) {
	r.begin()
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

		if r.Leases != nil {
			held, err := r.Leases.TakeLease(ctx, organization, opened.ID, r.claim())
			if err != nil {
				r.Logger.Error("an investigation's lease could not be taken",
					slog.String("investigation_id", opened.ID.String()),
					slog.String("error", err.Error()))
				return
			}
			if !held {
				// Somebody else is running it. Not an error, and not something to log
				// loudly: it is what the lease is for.
				return
			}
		}
		r.runLeased(ctx, organization, opened)
	}()
}

// Claim runs the claiming loop until the context ends: take one waiting investigation,
// run it, take the next.
//
// This is what makes a Conversation turn happen. A message opens an investigation with no
// lease — the handler answers immediately and does not wait for a model — and a worker
// picks it up here. It is also what lets a deployment add a replica: two claimers take
// different work, because the claim is a row lock and not a decision either of them makes.
func (r *Runner) Claim(ctx context.Context) {
	r.begin()
	for {
		claimed := r.claimOne(ctx)
		if ctx.Err() != nil {
			return
		}
		if claimed {
			// Something was waiting; there may be more. Look again rather than sleeping
			// through a queue.
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(claimInterval):
		}
	}
}

// claimOne takes and runs at most one investigation, reporting whether it found any.
func (r *Runner) claimOne(ctx context.Context) bool {
	if r.Leases == nil || r.AtCapacity() {
		return false
	}
	organization, claimed, took, err := r.Leases.ClaimInvestigation(ctx, r.claim())
	if err != nil {
		if ctx.Err() == nil {
			r.Logger.Error("investigations could not be claimed",
				slog.String("error", err.Error()))
		}
		return false
	}
	if !took {
		return false
	}

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
		runCtx, done := context.WithTimeout(r.base, investigationTimeout)
		defer done()
		r.runLeased(runCtx, organization, claimed)
	}()
	return true
}

// Sweep runs the recovery loop until the context ends: any investigation whose lease
// lapsed is FAILED with a stated reason and its stream is ended.
//
// It is not a resumption. Resuming would be a fiction — the model transcript is
// deliberately not persisted, only semantic events are — and an honest failure the operator
// can re-ask from is better than a fabricated continuation.
func (r *Runner) Sweep(ctx context.Context) {
	if r.Leases == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(sweepInterval):
		}
		recovered, err := r.Leases.RecoverStale(ctx, RecoveryReason, sweepBatch)
		switch {
		case err != nil && ctx.Err() == nil:
			r.Logger.Error("stale investigations could not be recovered",
				slog.String("error", err.Error()))
		case recovered > 0:
			// Counted rather than merely logged: a crash loop is only visible as a number.
			r.Telemetry.RecoveredStale(recovered)
			r.Logger.Warn("stale investigations were recovered",
				slog.Int("recovered", recovered),
				slog.String("reason", RecoveryReason))
		}
	}
}

// runLeased runs one investigation under a heartbeat, so a lease this process holds does
// not lapse while it is genuinely working.
func (r *Runner) runLeased(
	ctx context.Context, organization tenancy.Organization, opened Investigation,
) {
	if r.Leases == nil {
		r.run(ctx, organization, opened)
		return
	}
	// The wait is measured from when the record was created, so it counts the whole
	// distance between somebody asking and anybody starting — not just the part this
	// process could see.
	r.Telemetry.claimed(opened.CreatedAt)

	// The run takes the HEARTBEAT's context, not the parent. That is the whole point of
	// the heartbeat: when it discovers the lease is gone, cancelling this is what actually
	// stops the work. Handing the run the parent context — as this did — left a worker
	// that had lost its claim still reading, still spending, and still writing for an
	// investigation another worker now owned.
	held, lost := context.WithCancel(ctx)
	defer lost()
	go r.heartbeat(held, lost, organization, opened.ID)
	r.run(held, organization, opened)

	// The drain is deliberately given the PARENT context. It writes inside a detached
	// window anyway, and a turn that ended — however it ended — must not leave its
	// conversation holding messages nothing will pick up.
	r.drain(ctx, organization, opened)
}

// drain takes up whatever arrived while this turn was running.
//
// THIS is the "next safe point" the steering design turns on. A message sent mid-run is
// accepted and left queued rather than starting a second agent against the same context;
// here, once the turn has genuinely ended, everything queued becomes exactly ONE next turn.
// The new turn is unleased, so whichever worker claims it next runs it — including a
// different replica.
//
// It runs in a detached window for the same reason the final record write does: a
// cancelled run has still ended, and the conversation must not be left holding messages
// that nothing will ever pick up.
func (r *Runner) drain(
	ctx context.Context, organization tenancy.Organization, finished Investigation,
) {
	if finished.ConversationID == uuid.Nil {
		return
	}
	writeCtx, done := writeWindow(ctx)
	defer done()

	opened, err := r.Store.DrainConversation(writeCtx, organization,
		finished.ConversationID, r.WindowLead)
	if err != nil {
		// The messages stay queued and the next message or the next turn's end takes them
		// up. Losing the drain delays an answer; it does not lose what was said.
		r.Logger.Error("a conversation's queued messages could not be taken up",
			slog.String("conversation_id", finished.ConversationID.String()),
			slog.String("error", err.Error()))
		return
	}
	if opened {
		r.Logger.Info("a conversation turn was drained into a new investigation",
			slog.String("conversation_id", finished.ConversationID.String()))
	}
}

// heartbeat renews this worker's lease until the investigation ends. A renewal that
// matches no row means the lease is gone — swept, or the investigation already terminal —
// and the run is CANCELLED rather than left writing for work another worker now owns.
func (r *Runner) heartbeat(
	ctx context.Context, lost context.CancelFunc, organization tenancy.Organization,
	id uuid.UUID,
) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.heartbeatEvery()):
		}
		held, err := r.Leases.Heartbeat(ctx, organization, id, r.claim())
		if err != nil {
			// A database that could not be reached is not proof the lease is gone, and
			// giving up on the first failure would abandon work over one blip. The lease's
			// own expiry is the backstop.
			if ctx.Err() == nil {
				r.Logger.Warn("an investigation's lease could not be renewed",
					slog.String("investigation_id", id.String()),
					slog.String("error", err.Error()))
			}
			continue
		}
		if !held {
			r.Logger.Warn("an investigation's lease was lost; stopping this worker's run",
				slog.String("investigation_id", id.String()))
			lost()
			return
		}
	}
}

// heartbeatEvery is how often this runner renews a lease it holds.
func (r *Runner) heartbeatEvery() time.Duration {
	if r.HeartbeatEvery <= 0 {
		return heartbeatInterval
	}
	return r.HeartbeatEvery
}

// claim is what this worker asks for, with the defaults filled in.
func (r *Runner) claim() Claim {
	concurrent := r.OrgConcurrent
	if concurrent <= 0 {
		concurrent = maxConcurrent
	}
	return Claim{Worker: r.workerName(), LeaseFor: leaseDuration, OrgConcurrent: concurrent}
}

// workerName identifies this process among the live ones. A configured name wins; a
// derived one is the host and the process, which is enough to tell two replicas apart and
// enough to find one in a log.
func (r *Runner) workerName() string {
	r.identity.Do(func() {
		r.worker = r.Worker
		if r.worker != "" {
			return
		}
		host, err := os.Hostname()
		if err != nil || host == "" {
			host = "worker"
		}
		r.worker = fmt.Sprintf("%s/%d", host, os.Getpid())
	})
	return r.worker
}

// begin makes the process-lifetime context, once.
func (r *Runner) begin() {
	r.once.Do(func() {
		r.base, r.cancel = context.WithCancel(context.Background())
	})
}

// Drain stops taking the process down with investigations mid-flight: what is running is
// cancelled — each fails with the reason recorded — and waited for.
//
// A deploy is therefore not a crash. What this cannot cover is the process that is killed
// rather than asked to stop, and that is what the lease and its sweep are for.
func (r *Runner) Drain() {
	r.begin()
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
	// A windowed tool reports the window it ACTUALLY read, which is not what was asked
	// for whenever the clamp narrowed it — including a call phrased with no window at
	// all. A tool that reads no window leaves the investigation's own in place, so the
	// record always names the bound in force.
	if !result.WindowFrom.IsZero() && !result.WindowUntil.IsZero() {
		run.WindowFrom, run.WindowUntil = result.WindowFrom, result.WindowUntil
		run.WindowApplied = true
	}
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
		// The ending was refused, and the reason that matters is that somebody else has
		// already ended it — the sweeper, after this worker's lease lapsed. Its terminal
		// event is already on the stream, and writing a second one would tell a reader
		// the run finished twice. The event follows the record or it does not happen.
		r.Logger.Error("a failed investigation could not be recorded as failed",
			slog.String("investigation_id", id.String()),
			slog.String("error", err.Error()))
		return
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

// answerCutMark is what a truncated answer ends with. An answer that stops mid-sentence
// with no mark is indistinguishable from one that finished, and an operator acting on
// half a sentence is the failure this exists to prevent.
const answerCutMark = "… [truncated: the full account is in the findings]"

// boundedAnswer holds the direct answer inside its bound and says so when it cuts. The
// mark is charged against the bound rather than appended past it, so the result is always
// within the ceiling the record is written under.
func boundedAnswer(text string) string {
	runes := []rune(text)
	if len(runes) <= MaxAnswerLength {
		return text
	}
	mark := []rune(answerCutMark)
	return string(runes[:MaxAnswerLength-len(mark)]) + answerCutMark
}
