package reasoning

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

// Decider satisfies the investigation domain's model boundary against one configured
// deployment: render the brief, ask the provider, decode the document, price the call.
//
// It is built at startup, where a deployment nobody consented to, a model nobody priced
// or a provider nobody implements is a refusal the person who configured it reads —
// rather than a failed investigation at three in the morning.
type Decider struct {
	provider   Provider
	deployment Deployment
	rate       Rate
}

// NewDecider validates the deployment against consent and the rate table and binds it to
// its provider.
func NewDecider(
	deployment Deployment, provider Provider, tariff Tariff, consent Consent,
) (*Decider, error) {
	if !consent.Permits(deployment.Provider) {
		return nil, fmt.Errorf("%w: %s is configured and not consented to; evidence may "+
			"only be sent to providers named in the consent list",
			ErrNotConsented, deployment.Provider)
	}
	rate, err := tariff.Lookup(deployment.Provider, deployment.Model)
	if err != nil {
		return nil, err
	}
	return &Decider{provider: provider, deployment: deployment, rate: rate}, nil
}

// Decide asks the deployment for the next move. The completion is priced whatever
// happened: a refused or malformed answer still consumed tokens, and the investigation's
// spend must say so.
func (d *Decider) Decide(
	ctx context.Context, brief investigation.Brief,
) (investigation.Decision, error) {
	prompt := renderBrief(d.deployment, brief)

	decision := investigation.Decision{}
	// One in-deployment retry for an answer that broke its own schema or ran out of
	// output: the same model usually produces a well-formed document the second time.
	for attempt := 0; attempt < 2; attempt++ {
		completion, err := d.provider.Complete(ctx, prompt)
		decision.Spend = decision.Spend.Add(spendOf(d.rate, completion.Usage))
		if err != nil {
			return decision, err
		}

		switch completion.Stop {
		case StopRefused:
			return decision, Failed(OutcomeRefused, d.deployment.Provider, completion.Model,
				"the provider's safeguards declined the investigation prompt")
		case StopTruncated:
			continue
		}

		calls, findings, decodeErr := decodeDecision(completion.Document, len(brief.Runs))
		if decodeErr != nil {
			if attempt == 0 {
				continue
			}
			return decision, Failed(OutcomeMalformed, d.deployment.Provider,
				completion.Model, decodeErr.Error())
		}
		decision.Calls = calls
		decision.Findings = findings
		return decision, nil
	}
	return decision, Failed(OutcomeMalformed, d.deployment.Provider, d.deployment.Model,
		"the answer was truncated or malformed twice")
}

// decodeDecision reads the document against the declared shape and enforces the bounds
// the schema deliberately does not: citations must name runs that happened, and a
// statement must be sayable in an operator's screenful.
func decodeDecision(
	document []byte, runs int,
) ([]investigation.ToolCall, []investigation.Finding, error) {
	var decoded struct {
		Calls []struct {
			Tool      string         `json:"tool"`
			Arguments map[string]any `json:"arguments"`
		} `json:"calls"`
		Findings []struct {
			Statement string `json:"statement"`
			Sources   []int  `json:"sources"`
		} `json:"findings"`
	}
	if err := json.Unmarshal(document, &decoded); err != nil {
		return nil, nil, fmt.Errorf("the answer is not the declared document: %w", err)
	}

	// Calls win when both arrive: findings stated while asking for more reads are
	// findings the reads were meant to establish.
	if len(decoded.Calls) > 0 {
		calls := make([]investigation.ToolCall, 0, len(decoded.Calls))
		for _, call := range decoded.Calls {
			if call.Tool == "" {
				return nil, nil, fmt.Errorf("a call names no tool")
			}
			calls = append(calls, investigation.ToolCall{
				Tool: call.Tool, Arguments: call.Arguments,
			})
		}
		return calls, nil, nil
	}

	findings := make([]investigation.Finding, 0, len(decoded.Findings))
	for _, finding := range decoded.Findings {
		if finding.Statement == "" || len(finding.Statement) > maxStatementLength {
			return nil, nil, fmt.Errorf(
				"a finding's statement is empty or past %d characters", maxStatementLength)
		}
		if len(finding.Sources) == 0 {
			return nil, nil, fmt.Errorf("a finding cites no run at all")
		}
		for _, ordinal := range finding.Sources {
			if ordinal < 1 || ordinal > runs {
				return nil, nil, fmt.Errorf(
					"a finding cites run %d, and only %d ran", ordinal, runs)
			}
		}
		findings = append(findings, investigation.Finding{
			Statement: finding.Statement, Sources: finding.Sources,
		})
	}
	return nil, findings, nil
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
