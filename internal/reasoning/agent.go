package reasoning

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

// THE EXCHANGE DRIVER — the autonomous implementation of the investigation
// domain's Investigator boundary. One Exchange per investigation: the frozen preamble and
// the orientation render once and cache; each turn replays the transcript and appends
// the newest results; the provider's native tool calling carries the calls, and the
// conclude tool's call IS the conclusion. Every attempt is priced, a refusal is named
// and never read as an answer, and a malformed document gets exactly one in-deployment
// retry.

// Agent builds Exchanges against one configured deployment. Validated at startup:
// a deployment nobody consented to is a refusal whoever configured it reads, not a
// 03:00 failure.
type Agent struct {
	provider   Provider
	deployment Deployment
	rate       Rate
	telemetry  *Telemetry
}

// NewAgent validates the deployment against consent and the rate table and binds it to
// its provider.
func NewAgent(
	deployment Deployment, provider Provider, tariff Tariff, consent Consent,
) (*Agent, error) {
	if !consent.Permits(deployment.Provider) {
		return nil, fmt.Errorf("%w: %s is configured and not consented to; evidence may "+
			"only be sent to providers named in the consent list",
			ErrNotConsented, deployment.Provider)
	}
	rate, err := tariff.Lookup(deployment.Provider, deployment.Model)
	if err != nil {
		return nil, err
	}
	return &Agent{provider: provider, deployment: deployment, rate: rate}, nil
}

// Instrument attaches the per-call telemetry. Without it the agent still works and
// emits nothing, which is only right for tests.
func (a *Agent) Instrument(telemetry *Telemetry) { a.telemetry = telemetry }

// OpenExchange starts one investigation's Exchange. The tool definitions are
// generated here from the orientation's own sources — the one declarative contract,
// never a second hand-written representation — deduplicated by name, since two
// integrations of one type declare the same tools.
func (a *Agent) OpenExchange(
	_ context.Context, orientation investigation.Orientation,
) (investigation.Exchange, error) {
	return &exchange{
		agent:       a,
		orientation: renderOrientation(orientation),
		tools:       exchangeTools(orientation),
	}, nil
}

// exchange is one investigation's running exchange with the model. Not safe for concurrent use;
// one investigation runs its turns in sequence.
type exchange struct {
	agent       *Agent
	orientation string
	tools       []integrations.ToolDefinition
	turns       []Turn
	// opening is an instruction that arrived before any turn existed — an Exchange
	// forced to conclude from the start. It renders as a volatile block after the
	// cached orientation, because a transcript cannot open with an empty assistant
	// message.
	opening string
	// runs is the highest run ordinal fed back so far: the ordinal universe a
	// conclusion may cite. A maximum rather than a count, so a transcript whose
	// ordinals are not dense still cites only runs that happened.
	runs int
	// spent accumulates this Exchange's cost, for the spend-ceiling backstop. The
	// loop enforces the ceiling as product behavior; this refusal firing means a loop
	// forgot its own check.
	spent investigation.Spend
}

// Next feeds the previous move's results and asks for the next move.
func (e *exchange) Next(
	ctx context.Context, results []investigation.CallResult, mustConclude bool,
	reason string,
) (investigation.Move, error) {
	move := investigation.Move{}

	// The spend-ceiling backstop: the concluding turn is always
	// allowed, because a partial conclusion costs one more call and refusing it would
	// turn every reached ceiling into a failure.
	if ceiling := e.agent.deployment.SpendCeilingMicroCents; ceiling > 0 &&
		e.spent.MicroCents >= ceiling && !mustConclude {
		return move, Failed(OutcomeCeilingReached, e.agent.deployment.Provider,
			e.agent.deployment.Model, fmt.Sprintf(
				"the investigation has spent %d micro-cents against a ceiling of %d "+
					"and may only conclude", e.spent.MicroCents, ceiling))
	}

	e.appendResults(results)
	forced := mustConclude
	if forced {
		e.instruct(concludeInstruction(reason))
	}

	// One in-deployment retry for an answer that broke its contract, plus one forced
	// re-ask when a plain answer arrived instead of a call: the same model usually
	// produces a well-formed move the second time.
	for attempt := 0; attempt < 2; attempt++ {
		completion, err := e.agent.telemetry.complete(
			ctx, e.agent.provider, e.agent.deployment, e.agent.rate, e.prompt(forced))
		turnSpend := spendOf(e.agent.rate, completion.Usage)
		move.Spend = move.Spend.Add(turnSpend)
		e.spent = e.spent.Add(turnSpend)
		if err != nil {
			return move, err
		}

		switch completion.Stop {
		case StopRefused:
			return move, Failed(OutcomeRefused, e.agent.deployment.Provider,
				completion.Model, "the provider's safeguards declined the investigation")
		case StopTruncated:
			continue
		}

		reads, conclude := splitCalls(completion.ToolCalls)
		// Reads win when both arrive: findings stated while asking for more reads are
		// findings the reads were meant to establish. The forced turn accepts only the
		// conclusion.
		if len(reads) > 0 && !mustConclude {
			e.remember(completion)
			move.Calls = agentCalls(reads)
			return move, nil
		}
		if conclude != nil {
			conclusion, decodeErr := decodeConclusion(conclude.Arguments, e.runs)
			if decodeErr != nil {
				if attempt == 0 {
					continue
				}
				return move, Failed(OutcomeMalformed, e.agent.deployment.Provider,
					completion.Model, decodeErr.Error())
			}
			move.Conclusion = &conclusion
			return move, nil
		}

		// No usable call. A plain answer means the model is done reading: remember the
		// turn and re-ask once with the conclude tool forced.
		if attempt == 0 {
			e.remember(completion)
			e.instruct(concludeInstruction(reason))
			forced = true
			continue
		}
	}
	return move, Failed(OutcomeMalformed, e.agent.deployment.Provider,
		e.agent.deployment.Model,
		"the answer was truncated or carried no usable call twice")
}

// appendResults attaches the newest results to the turn that asked for them.
func (e *exchange) appendResults(results []investigation.CallResult) {
	if len(results) == 0 {
		return
	}
	rendered := make([]ToolResultTurn, 0, len(results))
	for _, result := range results {
		rendered = append(rendered, renderResult(result))
		if result.Run.Ordinal > e.runs {
			e.runs = result.Run.Ordinal
		}
	}
	if len(e.turns) == 0 {
		// Results with no assistant turn cannot happen through the loop; keeping them
		// as a bare turn preserves the transcript's honesty anyway.
		e.turns = append(e.turns, Turn{Results: rendered})
		return
	}
	e.turns[len(e.turns)-1].Results = append(e.turns[len(e.turns)-1].Results, rendered...)
}

// instruct appends trailing user text to the newest turn — the forced-conclusion
// reason, or the re-ask after a plain answer.
func (e *exchange) instruct(instruction string) {
	if len(e.turns) == 0 {
		e.opening = instruction
		return
	}
	e.turns[len(e.turns)-1].Instruction = instruction
}

// remember appends the completion as an assistant turn, verbatim where the vendor
// needs it verbatim.
func (e *exchange) remember(completion Completion) {
	e.turns = append(e.turns, Turn{Assistant: AssistantTurn{
		Text:  string(completion.Document),
		Calls: completion.ToolCalls,
		Raw:   completion.Raw,
	}})
}

// prompt renders this turn's request: the frozen preamble, the cached orientation, the
// transcript, and the tool set. The forced turn withdraws every tool but conclude —
// the strongest form of "no further reads": a read is not discouraged, it is not
// offered.
func (e *exchange) prompt(forced bool) Prompt {
	prompt := Prompt{
		Model:           e.agent.deployment.Model,
		System:          []Block{{Text: agentPreamble, Cache: true}},
		Content:         []Block{{Text: e.orientation, Cache: true}},
		Tools:           e.tools,
		Turns:           e.turns,
		MaxOutputTokens: e.agent.deployment.MaxOutputTokens,
		Effort:          e.agent.deployment.Effort,
	}
	if e.opening != "" {
		prompt.Content = append(prompt.Content, Block{Text: e.opening})
	}
	if forced {
		// exchangeTools puts conclude last, so the last definition IS conclude.
		prompt.Tools = e.tools[len(e.tools)-1:]
		prompt.ForceTool = ConcludeToolName
	}
	return prompt
}

// exchangeTools generates the native definitions: every offered tool once, then
// conclude last — the forced concluding turn depends on conclude's position. Definition
// order follows the orientation's stable source order, so the definition set — and the
// agent revision derived over it — is stable run to run.
func exchangeTools(
	orientation investigation.Orientation,
) []integrations.ToolDefinition {
	seen := map[string]bool{}
	var definitions []integrations.ToolDefinition
	for _, source := range orientation.Sources {
		for _, tool := range source.Tools {
			if seen[tool.Name] {
				continue
			}
			seen[tool.Name] = true
			definitions = append(definitions, tool.Definition())
		}
	}
	return append(definitions, ConcludeDefinition())
}

// splitCalls separates the conclude call from the reads.
func splitCalls(calls []CompletionCall) (reads []CompletionCall, conclude *CompletionCall) {
	for index, call := range calls {
		if call.Name == ConcludeToolName {
			if conclude == nil {
				conclude = &calls[index]
			}
			continue
		}
		reads = append(reads, call)
	}
	return reads, conclude
}

// agentCalls translates completion calls into the domain's shape, decoding each call's
// arguments. Arguments that are not an object reach the tool as empty and are refused
// there, where the refusal is a recorded run.
func agentCalls(calls []CompletionCall) []investigation.AgentCall {
	translated := make([]investigation.AgentCall, 0, len(calls))
	for _, call := range calls {
		arguments := map[string]any{}
		if len(call.Arguments) > 0 {
			_ = json.Unmarshal(call.Arguments, &arguments)
		}
		translated = append(translated, investigation.AgentCall{
			ID: call.ID, Tool: call.Name, Arguments: arguments,
		})
	}
	return translated
}

// decodeConclusion reads the conclude call against the conclusion contract and
// enforces the bounds the schema deliberately does not: citations must name runs that happened,
// kinds and confidences must be the declared vocabulary, and the texts must fit the
// record.
func decodeConclusion(document []byte, runs int) (investigation.Conclusion, error) {
	var decoded struct {
		Answer   string `json:"answer"`
		Findings []struct {
			Statement  string `json:"statement"`
			Kind       string `json:"kind"`
			Confidence string `json:"confidence"`
			Sources    []int  `json:"sources"`
		} `json:"findings"`
		NextSteps []string `json:"next_steps"`
	}
	if err := json.Unmarshal(document, &decoded); err != nil {
		return investigation.Conclusion{}, fmt.Errorf(
			"the conclusion is not the declared document: %w", err)
	}

	if len(decoded.Answer) > investigation.MaxAnswerLength {
		return investigation.Conclusion{}, fmt.Errorf(
			"the answer is past %d characters", investigation.MaxAnswerLength)
	}
	conclusion := investigation.Conclusion{Answer: decoded.Answer}
	for _, finding := range decoded.Findings {
		if finding.Statement == "" || len(finding.Statement) > maxStatementLength {
			return investigation.Conclusion{}, fmt.Errorf(
				"a finding's statement is empty or past %d characters", maxStatementLength)
		}
		if !oneOf(finding.Kind, investigation.FindingKinds) {
			return investigation.Conclusion{}, fmt.Errorf(
				"a finding's kind %q is not in the declared vocabulary", finding.Kind)
		}
		if !oneOf(finding.Confidence, investigation.Confidences) {
			return investigation.Conclusion{}, fmt.Errorf(
				"a finding's confidence %q is not confirmed, likely or possible",
				finding.Confidence)
		}
		if len(finding.Sources) == 0 {
			return investigation.Conclusion{}, fmt.Errorf("a finding cites no run at all")
		}
		for _, ordinal := range finding.Sources {
			if ordinal < 1 || ordinal > runs {
				return investigation.Conclusion{}, fmt.Errorf(
					"a finding cites run %d, and only %d ran", ordinal, runs)
			}
		}
		conclusion.Findings = append(conclusion.Findings, investigation.Finding{
			Statement: finding.Statement, Kind: finding.Kind,
			Confidence: finding.Confidence, Sources: finding.Sources,
		})
	}
	// The bounds are the record's own, enforced here because several providers
	// silently drop schema bounds; the loop bounds them again before the record.
	if len(decoded.NextSteps) > investigation.MaxConclusionNextSteps {
		return investigation.Conclusion{}, fmt.Errorf(
			"the conclusion recommends %d next steps, past %d",
			len(decoded.NextSteps), investigation.MaxConclusionNextSteps)
	}
	for _, step := range decoded.NextSteps {
		if step == "" || len(step) > investigation.MaxNextStepLength {
			return investigation.Conclusion{}, fmt.Errorf(
				"a next step is empty or past %d characters",
				investigation.MaxNextStepLength)
		}
	}
	conclusion.NextSteps = decoded.NextSteps
	return conclusion, nil
}

func oneOf(value string, allowed []string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

// maxStatementLength bounds one finding. Enforced here rather than in the schema because
// several providers silently drop schema bounds.
const maxStatementLength = 2048

// spendOf prices one call in the domain's own terms. Cache reads and writes are
// input-side tokens the provider handled, so they count as input.
func spendOf(rate Rate, usage TokenUsage) investigation.Spend {
	return investigation.Spend{
		InputTokens:  usage.Input.Or(0) + usage.CacheRead.Or(0) + usage.CacheWrite.Or(0),
		OutputTokens: usage.Output.Or(0),
		MicroCents:   rate.Cost(usage),
	}
}
