package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/secrets"
)

// Store is the durable state Agent.Run consumes.
type Store interface {
	InvestigationCandidates(context.Context, tenancy.Organization) ([]integrations.Integration, error)
	TriggerIncident(context.Context, tenancy.Organization, uuid.UUID) (investigation.Trigger, error)
	ConversationBrief(context.Context, tenancy.Organization, uuid.UUID, int) (investigation.Brief, error)
	WorkloadInventory(context.Context, tenancy.Organization, int) ([]string, error)
	RecordToolRun(context.Context, tenancy.Organization, uuid.UUID, investigation.ToolRun) error
	RecordCredentialUnseal(context.Context, tenancy.Organization, uuid.UUID, string) error
	AppendEvent(context.Context, tenancy.Organization, uuid.UUID, investigation.Event) error
	ConcludeInvestigation(context.Context, tenancy.Organization, uuid.UUID,
		investigation.Conclusion, string, investigation.Spend) error
	FailInvestigation(context.Context, tenancy.Organization, uuid.UUID, string, investigation.Spend) error
}

// Agent runs investigations against one validated model deployment.
type Agent struct {
	model      Model
	deployment Deployment
	rate       Rate
	telemetry  *Telemetry

	Store                  Store
	Catalog                integrations.Catalog
	Sealer                 seal.Sealer
	RuntimeTelemetry       *investigation.Telemetry
	Logger                 *slog.Logger
	MaxToolRuns            int
	MaxTurns               int
	ContextBudget          int
	ContextCeiling         int
	ModelName              string
	SpendCeilingMicroCents int64
}

// NewAgent validates the deployment against consent and the rate table and binds it to
// its provider.
func NewAgent(
	deployment Deployment, model Model, tariff Tariff, consent Consent,
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
	return &Agent{model: model, deployment: deployment, rate: rate}, nil
}

// Instrument attaches the per-call telemetry. Without it the agent still works and
// emits nothing, which is only right for tests.
func (a *Agent) Instrument(telemetry *Telemetry) { a.telemetry = telemetry }

// exchangeTools generates the native definitions: every offered tool once, then
// conclude last — the forced concluding turn depends on conclude's position. Definition
// order follows the orientation's stable source order, so the definition set — and the
// agent revision derived over it — is stable run to run.
func exchangeTools(
	orientation orientation,
) []integrations.ToolDefinition {
	seen := map[string]bool{}
	var definitions []integrations.ToolDefinition
	for _, source := range orientation.Sources {
		for _, tool := range source.Tools {
			if seen[tool.Name] {
				continue
			}
			seen[tool.Name] = true
			definitions = append(definitions, envelopeDefinition(tool.Definition()))
		}
	}
	definitions = append(definitions, UpdateHypothesesDefinition())
	return append(definitions, ConcludeDefinition())
}

func envelopeDefinition(definition integrations.ToolDefinition) integrations.ToolDefinition {
	definition.InputSchema = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"purpose": map[string]any{
				"type":        "string",
				"description": "Concise operator-visible reason for this read.",
			},
			"hypothesisId": map[string]any{
				"type":        "string",
				"description": "Stable visible hypothesis ID this read tests, when applicable.",
			},
			"input": definition.InputSchema,
		},
		"required":             []any{"input", "purpose"},
		"additionalProperties": false,
	}
	return definition
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
func agentCalls(calls []CompletionCall) []toolCall {
	translated := make([]toolCall, 0, len(calls))
	for _, call := range calls {
		arguments := map[string]any{}
		if len(call.Arguments) > 0 {
			_ = json.Unmarshal(call.Arguments, &arguments)
		}
		translatedCall := toolCall{ID: call.ID, Tool: call.Name}
		if call.Name == UpdateHypothesesToolName {
			translatedCall.Arguments = arguments
		} else {
			translatedCall.Purpose, _ = arguments["purpose"].(string)
			translatedCall.HypothesisID, _ = arguments["hypothesisId"].(string)
			translatedCall.Arguments, _ = arguments["input"].(map[string]any)
			if translatedCall.Arguments == nil {
				translatedCall.Arguments = map[string]any{}
			}
		}
		translated = append(translated, translatedCall)
	}
	return translated
}

// decodeConclusion reads the conclude call against the conclusion contract and
// enforces the bounds the schema deliberately does not: citations must name runs that happened,
// kinds and confidences must be the declared vocabulary, and the texts must fit the
// record.
func decodeConclusion(
	document []byte, runs int, _ bool,
) (investigation.Conclusion, error) {
	var decoded struct {
		Status  string `json:"status"`
		Summary string `json:"summary"`
		Impact  struct {
			Status           string   `json:"status"`
			CurrentState     string   `json:"current_state"`
			AffectedServices []string `json:"affected_services"`
			AffectedUsers    []string `json:"affected_users"`
			Summary          string   `json:"summary"`
			RunRefs          []int    `json:"run_refs"`
		} `json:"impact"`
		Findings []struct {
			ID         string `json:"id"`
			Statement  string `json:"statement"`
			Kind       string `json:"kind"`
			Confidence string `json:"confidence"`
			Mechanism  string `json:"mechanism"`
			RunRefs    []int  `json:"run_refs"`
		} `json:"findings"`
		Hypotheses []struct {
			ID, Statement, Status, Test string
			RunRefs                     []int `json:"run_refs"`
		} `json:"hypotheses"`
		Actions []struct {
			Title            string `json:"title"`
			Type             string `json:"type"`
			Rationale        string `json:"rationale"`
			Risk             string `json:"risk"`
			Verification     string `json:"verification"`
			Reversible       bool   `json:"reversible"`
			RequiresApproval bool   `json:"requires_approval"`
			RunRefs          []int  `json:"run_refs"`
		} `json:"actions"`
		Limitations []struct {
			Type, Statement string
			RunRefs         []int `json:"run_refs"`
		} `json:"limitations"`
	}
	if err := json.Unmarshal(document, &decoded); err != nil {
		return investigation.Conclusion{}, fmt.Errorf(
			"the conclusion is not the declared document: %w", err)
	}

	if strings.TrimSpace(decoded.Summary) == "" {
		return investigation.Conclusion{}, fmt.Errorf(
			"the conclusion summary is empty")
	}
	if !oneOf(decoded.Status, investigation.ConclusionStatuses) {
		return investigation.Conclusion{}, fmt.Errorf("invalid conclusion status %q", decoded.Status)
	}
	if !oneOf(decoded.Impact.Status, investigation.ImpactStatuses) {
		return investigation.Conclusion{}, fmt.Errorf("invalid impact status %q", decoded.Impact.Status)
	}
	if err := validateRunRefs(decoded.Impact.RunRefs, runs, "impact"); err != nil {
		return investigation.Conclusion{}, err
	}
	if decoded.Impact.Status != string(investigation.ImpactUnknown) &&
		len(decoded.Impact.RunRefs) == 0 {
		return investigation.Conclusion{}, fmt.Errorf("known or partial impact cites no run")
	}
	if decoded.Impact.Status == string(investigation.ImpactUnknown) &&
		(len(decoded.Impact.AffectedServices) > 0 || len(decoded.Impact.AffectedUsers) > 0 ||
			len(decoded.Impact.RunRefs) > 0 || decoded.Impact.CurrentState != "unknown" ||
			!unknownImpactSummary(decoded.Impact.Summary)) {
		return investigation.Conclusion{}, fmt.Errorf("unknown impact cannot claim state, affected entities, or evidence")
	}
	conclusion := investigation.Conclusion{
		Status: investigation.ConclusionStatus(decoded.Status), Summary: decoded.Summary,
		Impact: investigation.ImpactAssessment{
			Status:       investigation.ImpactStatus(decoded.Impact.Status),
			CurrentState: decoded.Impact.CurrentState, AffectedServices: decoded.Impact.AffectedServices,
			AffectedUsers: decoded.Impact.AffectedUsers, Summary: decoded.Impact.Summary,
			RunRefs: decoded.Impact.RunRefs,
		},
	}
	for _, finding := range decoded.Findings {
		if finding.ID == "" || finding.Statement == "" || len(finding.Statement) > maxStatementLength {
			return investigation.Conclusion{}, fmt.Errorf(
				"a finding's id or statement is empty, or its statement is past %d characters", maxStatementLength)
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
		if len(finding.RunRefs) == 0 {
			return investigation.Conclusion{}, fmt.Errorf("a finding cites no run at all")
		}
		if err := validateRunRefs(finding.RunRefs, runs, "finding"); err != nil {
			return investigation.Conclusion{}, err
		}
		if causalFinding(finding.Kind) && strings.TrimSpace(finding.Mechanism) == "" {
			return investigation.Conclusion{}, fmt.Errorf("causal finding %q has no mechanism", finding.ID)
		}
		conclusion.Findings = append(conclusion.Findings, investigation.Finding{
			ID: finding.ID, Statement: finding.Statement, Kind: finding.Kind,
			Confidence: finding.Confidence, Mechanism: finding.Mechanism, Sources: finding.RunRefs,
		})
	}
	for _, hypothesis := range decoded.Hypotheses {
		if hypothesis.ID == "" || hypothesis.Statement == "" || hypothesis.Test == "" ||
			!oneOf(hypothesis.Status, investigation.HypothesisStatuses) {
			return investigation.Conclusion{}, fmt.Errorf("a hypothesis is incomplete or has an invalid status")
		}
		if err := validateRunRefs(hypothesis.RunRefs, runs, "hypothesis"); err != nil {
			return investigation.Conclusion{}, err
		}
		conclusion.Hypotheses = append(conclusion.Hypotheses, investigation.HypothesisResult{
			ID: hypothesis.ID, Statement: hypothesis.Statement,
			Status: investigation.HypothesisStatus(hypothesis.Status), Test: hypothesis.Test,
			RunRefs: hypothesis.RunRefs,
		})
	}
	if len(decoded.Actions) > investigation.MaxConclusionActions {
		return investigation.Conclusion{}, fmt.Errorf("the conclusion proposes %d actions, past %d",
			len(decoded.Actions), investigation.MaxConclusionActions)
	}
	for _, action := range decoded.Actions {
		if action.Title == "" || action.Rationale == "" || action.Verification == "" ||
			len(action.Title) > investigation.MaxActionTextLength ||
			!oneOf(action.Type, investigation.ActionTypes) || !oneOf(action.Risk, investigation.ActionRisks) {
			return investigation.Conclusion{}, fmt.Errorf(
				"an action is incomplete, invalid, or past %d characters", investigation.MaxActionTextLength)
		}
		if err := validateRunRefs(action.RunRefs, runs, "action"); err != nil {
			return investigation.Conclusion{}, err
		}
		if len(action.RunRefs) == 0 {
			return investigation.Conclusion{}, fmt.Errorf("action %q cites no run", action.Title)
		}
		if stateChangingAction(action.Type) && !action.RequiresApproval {
			return investigation.Conclusion{}, fmt.Errorf("state-changing action %q does not require approval", action.Title)
		}
		conclusion.Actions = append(conclusion.Actions, investigation.ActionProposal{
			Title: action.Title, Type: investigation.ActionType(action.Type), Rationale: action.Rationale,
			Risk: investigation.ActionRisk(action.Risk), Reversible: action.Reversible,
			RequiresApproval: action.RequiresApproval, Verification: action.Verification,
			RunRefs: action.RunRefs,
		})
	}
	for _, limitation := range decoded.Limitations {
		if limitation.Statement == "" || !oneOf(limitation.Type, investigation.LimitationTypes) {
			return investigation.Conclusion{}, fmt.Errorf("a limitation is incomplete or has an invalid type")
		}
		if err := validateRunRefs(limitation.RunRefs, runs, "limitation"); err != nil {
			return investigation.Conclusion{}, err
		}
		conclusion.Limitations = append(conclusion.Limitations, investigation.Limitation{
			Type: investigation.LimitationType(limitation.Type), Statement: limitation.Statement,
			RunRefs: limitation.RunRefs,
		})
	}
	if err := validateConclusionStatus(conclusion); err != nil {
		return investigation.Conclusion{}, err
	}
	return conclusion, nil
}

func validateRunRefs(refs []int, runs int, owner string) error {
	for _, ordinal := range refs {
		if ordinal < 1 || ordinal > runs {
			return fmt.Errorf("a %s cites run %d, and only %d ran", owner, ordinal, runs)
		}
	}
	return nil
}

func causalFinding(kind string) bool {
	return kind == investigation.FindingCause || kind == investigation.FindingTrigger ||
		kind == investigation.FindingContributingFactor || kind == investigation.FindingPropagation
}

func stateChangingAction(kind string) bool {
	return kind == string(investigation.ActionMitigate) || kind == string(investigation.ActionRollback) ||
		kind == string(investigation.ActionFix)
}

func validateConclusionStatus(conclusion investigation.Conclusion) error {
	confirmedCause := false
	for _, finding := range conclusion.Findings {
		if finding.Kind == investigation.FindingCause && finding.Confidence == investigation.ConfidenceConfirmed {
			confirmedCause = true
		}
	}
	switch conclusion.Status {
	case investigation.VerifiedCause:
		if !confirmedCause {
			return fmt.Errorf("verified_cause requires a confirmed cited cause with a mechanism")
		}
	case investigation.SupportedExplanation:
		explanation := false
		for _, finding := range conclusion.Findings {
			explanation = explanation || finding.Kind == investigation.FindingCause ||
				finding.Kind == investigation.FindingTrigger ||
				finding.Kind == investigation.FindingContributingFactor
		}
		if !explanation {
			return fmt.Errorf("supported_explanation requires a cited explanatory finding")
		}
		alternative := false
		for _, hypothesis := range conclusion.Hypotheses {
			alternative = alternative || hypothesis.Status == investigation.HypothesisExploring ||
				hypothesis.Status == investigation.HypothesisUnresolved
		}
		if !alternative {
			return fmt.Errorf("supported_explanation requires a plausible remaining alternative")
		}
	case investigation.Inconclusive:
		if confirmedCause {
			return fmt.Errorf("inconclusive cannot carry a confirmed cause")
		}
	case investigation.AnswerOnly:
		for _, finding := range conclusion.Findings {
			if finding.Kind != investigation.FindingObservation && finding.Kind != investigation.FindingRuledOut {
				return fmt.Errorf("answer_only cannot carry causal findings")
			}
		}
	}
	return nil
}

func unknownImpactSummary(summary string) bool {
	switch strings.ToLower(strings.TrimSpace(summary)) {
	case "impact is unknown.", "impact is not established.", "impact was not established.",
		"impact was not assessed for this operator question.":
		return true
	default:
		return false
	}
}

func oneOf(value string, allowed []string) bool {
	return slices.Contains(allowed, value)
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
