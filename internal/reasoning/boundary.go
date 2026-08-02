package reasoning

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

// THE SHARED ORCHESTRATION: ONE IMPLEMENTATION OF THE MODEL BOUNDARY, WHATEVER ANSWERS IT.
//
// Everything a vendor does not decide happens here — rendering, the schema, decoding, the retry,
// the fallback chain, the cost, the record. That is what keeps a second adapter to one directory,
// and it is why the domain never learns which vendor is configured.

// Service implements the investigation capability's model boundary.
type Service struct {
	chain   []*deployed
	tariff  Tariff
	consent Consent
	ceiling *Ceiling
	logger  *slog.Logger
	now     func() time.Time
	// observe receives every completed call. It is where telemetry and audit entries are written,
	// and it is the only place a record reaches that survives a failed round — the domain drops
	// the usage on a call that returned an error, so without this a refused round's real spend
	// would go unrecorded.
	observe func(Record)

	// answered is the provider and model that most recently replied, which is not necessarily the
	// one configured.
	answeredMutex sync.Mutex
	answered      string
}

// deployed is one configured provider with the bounds it runs under.
type deployed struct {
	provider   Provider
	deployment Deployment
	breaker    *breaker
	limiter    *limiter
	rate       Rate
}

// Options assembles a service.
type Options struct {
	// Primary is the deployment that answers. Fallbacks are tried in order and only for outcomes
	// another deployment could plausibly answer.
	Primary   Provider
	Fallbacks []Provider
	// Deployments carries the configuration each provider was built from, in the same order:
	// primary first, then the fallbacks.
	Deployments []Deployment
	Tariff      Tariff
	Consent     Consent
	Ceiling     *Ceiling
	Logger      *slog.Logger
	Now         func() time.Time
	Observe     func(Record)
}

// New assembles the boundary from configured providers, refusing anything that could not work.
//
// Every model in the chain is priced here rather than on first use. An unpriced model would report
// a cost of zero, which silently disables the cost ceiling, and the failure would first be noticed
// as a bill — so it is moved to startup, where the person who chose the model is still the person
// reading the error.
func New(options Options) (*Service, error) {
	if options.Primary == nil {
		return nil, errors.New("a reasoning service needs a provider to answer with")
	}
	providers := append([]Provider{options.Primary}, options.Fallbacks...)
	if len(options.Deployments) != len(providers) {
		return nil, fmt.Errorf(
			"the reasoning service was given %d providers and %d deployments; each provider is "+
				"configured by exactly one", len(providers), len(options.Deployments))
	}

	now := options.Now
	if now == nil {
		now = time.Now
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	service := &Service{
		tariff:  options.Tariff,
		consent: options.Consent,
		ceiling: options.Ceiling,
		logger:  logger,
		now:     now,
		observe: options.Observe,
	}
	for index, provider := range providers {
		deployment := options.Deployments[index].WithDefaults()
		if deployment.Provider != provider.Name() {
			return nil, fmt.Errorf(
				"deployment %d configures %q but the provider in that position is %q",
				index, deployment.Provider, provider.Name())
		}
		// Consent is checked at startup as well as per hop, so a deployment nobody agreed to is a
		// refusal to start rather than a round that fails at 03:00.
		if !service.consent.Permits(deployment.Provider) {
			return nil, fmt.Errorf("%w: %s is configured but not consented to",
				ErrNotConsented, deployment.Provider)
		}
		rate, err := options.Tariff.Lookup(deployment.Provider, deployment.Model)
		if err != nil {
			return nil, err
		}
		service.chain = append(service.chain, &deployed{
			provider:   provider,
			deployment: deployment,
			breaker:    newBreaker(now),
			limiter:    newLimiter(deployment.MaxConcurrent),
			rate:       rate,
		})
	}
	return service, nil
}

// Versions is what a round opened against this service is pinned with. The model is named as
// provider and model together, because two vendors can and do ship models with the same family
// name and a transcript that could not tell them apart would replay against the wrong one.
//
// This is what was CONFIGURED, and it has to be, because a round is opened before anything has
// answered it — a recording is checked against these versions before the first call is made. What
// actually answered is a different question, and Answered reports it once there is an answer.
func (s *Service) Versions(planner, investigator string) investigation.Versions {
	primary := s.chain[0].deployment
	return investigation.Versions{
		Planner:       planner,
		Model:         primary.Provider + "/" + primary.Model,
		PromptVersion: PromptVersion,
		SchemaVersion: SchemaVersion,
		Investigator:  investigator,
	}
}

// Answered names the provider and model that most recently replied, or empty before anything has.
//
// It exists because the configured model and the answering model are not the same fact, and it is
// the second one a transcript has to be keyed on: a recording made by a fallback and keyed on the
// deployment that gave way would replay against a recording a different model produced. The
// recorder reads this when it writes the key.
func (s *Service) Answered() string {
	s.answeredMutex.Lock()
	defer s.answeredMutex.Unlock()
	return s.answered
}

// Describe renders the configured chain for a startup log, so an operator can see what will
// actually answer rather than reading configuration back to themselves.
func (s *Service) Describe() string {
	described := ""
	for index, hop := range s.chain {
		if index > 0 {
			described += " then "
		}
		described += hop.deployment.String() + " [" + hop.provider.Support().Describe() + "]"
	}
	return described
}

// Hypotheses proposes competing explanations from the brief alone.
func (s *Service) Hypotheses(
	ctx context.Context, brief investigation.Brief,
) (investigation.Hypothesized, error) {
	deliberation := investigation.Deliberation{Brief: brief}
	answer := investigation.Hypothesized{}
	usage, err := s.answer(ctx, exchange{
		method:       "hypotheses",
		deliberation: deliberation,
		schema:       HypothesesSchema(),
		task:         hypothesesTask,
		decode: func(document []byte) error {
			hypotheses, err := decodeHypotheses(document)
			if err != nil {
				return err
			}
			answer.Hypotheses = hypotheses
			return nil
		},
	})
	answer.Usage = usage
	return answer, err
}

// Requests proposes further reads from the hypotheses currently held.
func (s *Service) Requests(
	ctx context.Context, over investigation.Deliberation,
) (investigation.Proposed, error) {
	answer := investigation.Proposed{}
	usage, err := s.answer(ctx, exchange{
		method:       "requests",
		deliberation: over,
		schema:       ProposalsSchema(),
		task:         proposalsTask,
		decode: func(document []byte) error {
			proposed, err := decodeProposals(document, over)
			if err != nil {
				return err
			}
			answer.Proposals = proposed.Proposals
			answer.Hypotheses = proposed.Hypotheses
			answer.Weighings = proposed.Weighings
			answer.Settlings = proposed.Settlings
			return nil
		},
	})
	answer.Usage = usage
	return answer, err
}

// Conclude states the most supported explanation or abstains.
func (s *Service) Conclude(
	ctx context.Context, over investigation.Deliberation,
) (investigation.Concluded, error) {
	answer := investigation.Concluded{}
	usage, err := s.answer(ctx, exchange{
		method:       "conclude",
		deliberation: over,
		schema:       ConclusionSchema(),
		task:         conclusionTask,
		decode: func(document []byte) error {
			concluded, err := decodeConclusion(document, over)
			if err != nil {
				return err
			}
			answer.Draft = concluded.Draft
			answer.Hypotheses = concluded.Hypotheses
			answer.Weighings = concluded.Weighings
			answer.Settlings = concluded.Settlings
			return nil
		},
	})
	answer.Usage = usage
	return answer, err
}

// exchange is one call to the boundary, in the terms this package makes it.
type exchange struct {
	method       string
	deliberation investigation.Deliberation
	schema       Schema
	task         string
	decode       func(document []byte) error
}

// schemaAttempts is how many times one exchange may be asked for.
//
// Exactly two. A single bad generation should not lose a round, and an unbounded retry loop spends
// a cost ceiling quietly — those two facts bracket the number and there is nothing else to tune.
const schemaAttempts = 2

// answer runs one exchange to a decoded result, retrying a malformed answer once.
//
// The usage it returns is everything the exchange consumed, including the attempts that failed.
// A retry is not free and a round that hid the cost of its retries would be one nobody could price.
func (s *Service) answer(
	ctx context.Context, call exchange,
) (investigation.Usage, error) {
	spent := investigation.Usage{}
	var lastMalformed error

	for attempt := range schemaAttempts {
		document, record, err := s.ask(ctx, call)
		spent.Tokens += record.Usage.Billable()
		spent.MicroCents += record.MicroCents
		if err != nil {
			return spent, err
		}

		if decodeErr := call.decode(document); decodeErr != nil {
			lastMalformed = s.named(decodeErr, record)
			s.logger.WarnContext(ctx, "a model answer did not satisfy the declared schema",
				slog.String("method", call.method),
				slog.String("provider", record.Provider),
				slog.String("model", record.AnsweringModel),
				slog.Int("attempt", attempt+1),
				slog.String("reason", lastMalformed.Error()))
			continue
		}
		return spent, nil
	}
	return spent, lastMalformed
}

// named fills a decode failure in with the deployment that produced it. The decoder knows what was
// wrong with the document and nothing about who wrote it.
func (s *Service) named(err error, record Record) error {
	var failure *Failure
	if errors.As(err, &failure) {
		failure.Provider = record.Provider
		failure.Model = record.AnsweringModel
	}
	return err
}

// ask sends one exchange down the configured chain and returns the first answer it gets.
//
// The chain is explicit configuration. There is no implicit substitution: an operator who
// configured one deployment gets exactly that deployment and an honest failure. Crossing to the
// next hop is permitted only for an outcome another deployment could plausibly answer, only when
// that hop is consented to, and it is always recorded — so a vendor is never switched silently and
// the record always says who actually replied.
func (s *Service) ask(ctx context.Context, call exchange) ([]byte, Record, error) {
	system, content := blocks(call.deliberation, call.task)
	var lastErr error
	var lastRecord Record
	// The hop that was actually TRIED and gave way, which is not the one before this in the chain:
	// a hop skipped for want of consent or with its breaker open was never asked for anything, and
	// naming it as what gave way would attribute a fallback to a provider that was never called.
	var gaveWay string

	for index, hop := range s.chain {
		if !s.consent.Permits(hop.deployment.Provider) {
			// Checked per hop rather than once at the head of the chain: consenting to one vendor
			// is not consenting to the next, and a fallback that inherited consent would be an
			// undisclosed subprocessor change performed automatically.
			lastErr = Failed(OutcomeNotConsented, hop.deployment.Provider, hop.deployment.Model,
				"this deployment has not consented to sending evidence to this provider")
			continue
		}
		if !hop.breaker.Closed() {
			lastErr = Failed(OutcomeOutage, hop.deployment.Provider, hop.deployment.Model,
				"the circuit breaker for this provider is open after repeated failures")
			s.logger.WarnContext(ctx, "a model provider was skipped with its breaker open",
				slog.String("provider", hop.deployment.Provider),
				slog.String("model", hop.deployment.Model))
			continue
		}
		if !s.ceiling.Allows() {
			// A reached ceiling stops the whole chain rather than moving down it. A cheaper model
			// is still spending money that has run out.
			return nil, lastRecord, Failed(OutcomeCeilingReached,
				hop.deployment.Provider, hop.deployment.Model,
				fmt.Sprintf("the reasoning cost ceiling of %d micro-cents was reached",
					s.ceiling.Spent()))
		}

		document, record, err := s.send(ctx, hop, call, system, content)
		record.Method = call.method
		record.FellBack = gaveWay != ""
		record.FellBackFrom = gaveWay
		s.record(record)
		lastRecord = record
		gaveWay = hop.deployment.Provider + "/" + hop.deployment.Model

		if err == nil {
			hop.breaker.Succeeded()
			return document, record, nil
		}

		outcome, named := OutcomeOf(err)
		if named {
			hop.breaker.Failed(outcome)
		}
		lastErr = err
		if !named || !outcome.Recoverable() || index == len(s.chain)-1 {
			// A rejected request is this build's own defect and every hop would reject it
			// identically; a malformed answer is retried inside the deployment that produced it.
			// Neither is worth paying another vendor to fail at.
			return nil, record, err
		}
		s.logger.WarnContext(ctx, "a model deployment gave way to the next configured one",
			slog.String("provider", hop.deployment.Provider),
			slog.String("model", hop.deployment.Model),
			slog.String("outcome", outcome.String()),
			slog.String("reason", err.Error()))
	}

	if lastErr == nil {
		lastErr = Failed(OutcomeOutage, "", "", "no configured deployment was able to answer")
	}
	return nil, lastRecord, lastErr
}

// send performs one call against one deployment and prices it.
func (s *Service) send(
	ctx context.Context, hop *deployed, call exchange, system, content []Block,
) ([]byte, Record, error) {
	prompt := Prompt{
		Model:           hop.deployment.Model,
		System:          system,
		Content:         content,
		Schema:          call.schema,
		MaxOutputTokens: hop.deployment.MaxOutputTokens,
		Effort:          hop.deployment.Effort,
	}
	record := Record{
		Provider:       hop.deployment.Provider,
		RequestedModel: hop.deployment.Model,
		AnsweringModel: hop.deployment.Model,
		PromptVersion:  PromptVersion,
		SchemaVersion:  SchemaVersion,
	}

	if err := hop.limiter.Acquire(ctx); err != nil {
		return nil, record, Failed(OutcomeTimeout, hop.deployment.Provider, hop.deployment.Model,
			"the round ended while waiting for capacity to reach this provider")
	}
	defer hop.limiter.Release()

	if err := s.measure(ctx, hop, prompt); err != nil {
		return nil, record, err
	}

	started := s.now()
	completion, err := hop.provider.Complete(ctx, prompt)
	record.Latency = s.now().Sub(started)

	// The completion is read even on failure. A refused or truncated request still consumed
	// tokens, still names a model and still carries a request identifier, and all three belong in
	// the record whatever the outcome was.
	if completion.Model != "" {
		record.AnsweringModel = completion.Model
	}
	record.RequestID = completion.RequestID
	record.Usage = completion.Usage
	record.Stop = completion.Stop
	record.MicroCents = hop.rate.Cost(completion.Usage)
	s.ceiling.Record(record.MicroCents)

	if err != nil {
		return nil, record, err
	}
	return completion.Document, record, nil
}

// measure refuses an oversized deliberation before it is sent, where the provider can price one.
//
// Where the provider cannot, the check is SKIPPED and logged as skipped rather than passing
// silently. A capability that is absent is never reported as satisfied, and an operator reading
// this log line can tell the difference between a prompt that fitted and one nobody measured.
func (s *Service) measure(ctx context.Context, hop *deployed, prompt Prompt) error {
	if hop.deployment.MaxPromptTokens <= 0 {
		return nil
	}
	if !hop.provider.Support().TokenCounting {
		s.logger.DebugContext(ctx, "a prompt size bound was not checked",
			slog.String("provider", hop.deployment.Provider),
			slog.String("reason", "this provider cannot count tokens before sending"))
		return nil
	}

	counted, err := hop.provider.Measure(ctx, prompt)
	if err != nil {
		return err
	}
	if counted.Reported && counted.Tokens > hop.deployment.MaxPromptTokens {
		return Failed(OutcomeRejected, hop.deployment.Provider, hop.deployment.Model,
			fmt.Sprintf("the deliberation is %d tokens and this deployment allows %d",
				counted.Tokens, hop.deployment.MaxPromptTokens))
	}
	return nil
}

// record publishes one completed call.
//
// Identifiers, counts, outcomes and reasons travel. The text a customer's systems produced does
// not, and neither does the credential.
func (s *Service) record(entry Record) {
	if entry.AnsweringModel != "" {
		s.answeredMutex.Lock()
		s.answered = entry.Provider + "/" + entry.AnsweringModel
		s.answeredMutex.Unlock()
	}
	if s.observe != nil {
		s.observe(entry)
	}
	s.logger.Info("a model provider answered",
		slog.String("method", entry.Method),
		slog.String("provider", entry.Provider),
		slog.String("requested_model", entry.RequestedModel),
		slog.String("answering_model", entry.AnsweringModel),
		slog.String("request_id", entry.RequestID),
		slog.Int64("input_tokens", entry.Usage.Input.Or(0)),
		slog.Int64("output_tokens", entry.Usage.Output.Or(0)),
		slog.Int64("cache_write_tokens", entry.Usage.CacheWrite.Or(0)),
		slog.Int64("cache_read_tokens", entry.Usage.CacheRead.Or(0)),
		slog.Int64("reasoning_tokens", entry.Usage.Reasoning.Or(0)),
		slog.Bool("reasoning_reported", entry.Usage.Reasoning.Reported),
		slog.Bool("fell_back", entry.FellBack),
		slog.String("stop", entry.Stop.String()),
		slog.Int64("micro_cents", entry.MicroCents),
		slog.Duration("latency", entry.Latency),
		slog.String("prompt_version", entry.PromptVersion),
		slog.String("schema_version", entry.SchemaVersion))
}
