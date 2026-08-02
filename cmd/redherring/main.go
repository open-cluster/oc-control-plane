// Command redherring points a live model provider at exactly one investigation and reports what
// happened, in enough detail to score it.
//
// It exists because unit tests passing is not evidence that this works. The suite proves what the
// adapters send and what they do with what comes back; it cannot prove that the prompt elicits
// usable reasoning, that the schemas ask for the right things, or that a real model refuses to be
// led. Those are empirical, and this is the smallest instrument that answers them.
//
// # What this is NOT
//
// It is not the scenario harness. It provisions no clusters and drives no Relay: the evidence it
// serves is PRE-BAKED and stated as such, so nothing here demonstrates that the gathering pipeline
// works end to end. What it demonstrates is the model boundary — the prompt, the schemas, the
// decoding, the scope invariants, the cost accounting and the recording — against a real provider.
// The harness is what proves the rest, and this is what unblocks the harness.
//
// # The scenario
//
// A red herring, chosen because it is where a change-aware investigator most plausibly fails and
// where a wrong answer is most informative. A deployment's image was updated half an hour before
// the incident, which is the obvious thing to blame; the evidence says the pods stayed healthy for
// thirty minutes afterwards and then began failing to authenticate against their database, which
// is a credential rotation and not the deploy. An investigator that blames the deploy has been led
// by proximity. One that abstains has been too cautious with evidence that does point somewhere.
//
// # Running it
//
//	redherring -provider anthropic -model claude-opus-5 -key-file ./anthropic.key
//	redherring -provider zai       -model glm-4.7       -key-file ./zai.key
//
// The credential is read from the file named, never from an environment value, for the same reason
// every other secret in this program is: an environment value is readable from a process listing
// and appears in every diagnostic dump of the environment.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/reasoning"
	"github.com/open-cluster/oc-control-plane/internal/reasoning/providers"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "\nthe run did not complete: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	provider := flag.String("provider", "anthropic", "which provider answers")
	model := flag.String("model", "claude-opus-5", "the exact model identifier")
	keyFile := flag.String("key-file", "", "path to a file holding the API credential")
	effort := flag.String("effort", "high", "how hard to think: low, medium, high, xhigh, max")
	baseURL := flag.String("base-url", "", "override the provider's host")
	transcriptPath := flag.String("transcript", "", "write the recorded transcript here")
	deadline := flag.Duration("deadline", 10*time.Minute, "wall clock for the whole round")
	// One call, not the whole round. A model that thinks before answering can sit well past the
	// default on a prompt this size, and a timeout that fires on a working provider reports an
	// outage that never happened.
	requestTimeout := flag.Duration(
		"request-timeout", 5*time.Minute, "wall clock for a single call to the provider")
	flag.Parse()

	if *keyFile == "" {
		return errors.New("-key-file is required: the credential is read from a file so that it " +
			"cannot leak through a process listing")
	}
	credential, err := os.ReadFile(*keyFile)
	if err != nil {
		// The path is named and the contents never are.
		return fmt.Errorf("the credential file %s could not be read", *keyFile)
	}

	deployment := reasoning.Deployment{
		Provider:   *provider,
		Model:      *model,
		Effort:     reasoning.Effort(*effort),
		BaseURL:    *baseURL,
		Credential: reasoning.Secret(strings.TrimSpace(string(credential))),
		// Generous, because thinking and answer text share this bound on some models and a value
		// sized around the answer alone truncates mid-thought.
		MaxOutputTokens: 32_000,
		RequestTimeout:  *requestTimeout,
	}.WithDefaults()

	opened, err := providers.Open(deployment, providers.Options{})
	if err != nil {
		return err
	}

	observed := &observer{}
	service, err := reasoning.New(reasoning.Options{
		Primary:     opened,
		Deployments: []reasoning.Deployment{deployment},
		Tariff:      reasoning.DefaultTariff(),
		// One provider, consented to explicitly. There is no fallback configured, so a failure is
		// an honest failure rather than a second vendor nobody chose.
		Consent: reasoning.ConsentTo(deployment.Provider),
		Observe: observed.add,
	})
	if err != nil {
		return err
	}

	recorder := reasoning.Recording(service)
	ctx, cancel := context.WithTimeout(context.Background(), *deadline)
	defer cancel()

	report := &report{
		deployment: deployment,
		support:    opened.Support(),
		started:    time.Now(),
	}
	investigate(ctx, recorder, report)
	report.finished = time.Now()
	report.records = observed.all()
	report.print()

	if *transcriptPath != "" {
		versions := service.Versions("bounded-adaptive-v1", "redherring")
		encoded, marshalErr := json.MarshalIndent(recorder.Transcript(versions), "", "  ")
		if marshalErr != nil {
			return marshalErr
		}
		if writeErr := os.WriteFile(*transcriptPath, encoded, 0o600); writeErr != nil {
			return writeErr
		}
		fmt.Printf("\nTranscript written to %s\n", *transcriptPath)
	}
	if report.failed != nil {
		return report.failed
	}
	return nil
}

// investigate drives the three calls the boundary makes, serving the pre-baked evidence when the
// planner asks for a read.
func investigate(ctx context.Context, reasoner investigation.Reasoner, out *report) {
	brief := redHerringBrief()

	proposed, err := reasoner.Hypotheses(ctx, brief)
	out.hypotheses = proposed.Hypotheses
	if err != nil {
		out.failed = fmt.Errorf("proposing hypotheses: %w", err)
		return
	}

	deliberation := investigation.Deliberation{
		Brief:      brief,
		Hypotheses: proposed.Hypotheses,
		Available:  brief.Available,
		Remaining:  6,
		Pass:       1,
	}

	planned, err := reasoner.Requests(ctx, deliberation)
	out.proposals = planned.Proposals
	out.weighings = append(out.weighings, planned.Weighings...)
	out.settlings = append(out.settlings, planned.Settlings...)
	if err != nil {
		out.failed = fmt.Errorf("proposing reads: %w", err)
		return
	}

	// The reads are SERVED rather than dispatched. Every proposal is validated against the case's
	// scope exactly as the runner would validate it, so the invariant is exercised even though
	// nothing reaches a cluster.
	served, refused := serve(planned.Proposals, brief)
	out.served = served
	out.refusedReads = refused

	deliberation.Evidence = served
	deliberation.Gaps = redHerringGaps()
	deliberation.Remaining = 6 - len(planned.Proposals)
	deliberation.Pass = 2

	concluded, err := reasoner.Conclude(ctx, deliberation)
	out.draft = concluded.Draft
	out.weighings = append(out.weighings, concluded.Weighings...)
	out.settlings = append(out.settlings, concluded.Settlings...)
	if err != nil {
		out.failed = fmt.Errorf("concluding: %w", err)
		return
	}

	// The output schema runs before anything would be persisted, so a draft carrying an uncited
	// claim is refused here exactly as it would be in a real round.
	outcome, admitErr := investigation.AdmitOutcome(
		concluded.Draft, served, deliberation.Gaps, live(deliberation.Hypotheses, out.settlings))
	if admitErr != nil {
		out.admissionFailure = admitErr
		return
	}
	out.outcome = &outcome
}

// serve answers the planner's reads from the pre-baked evidence, refusing anything the case's own
// scope would refuse.
func serve(
	proposals []investigation.Proposal, brief investigation.Brief,
) ([]investigation.Item, []string) {
	bounds := investigation.Bounds{
		Scope: investigation.Scope{
			Namespace:    brief.Resource.Namespace,
			WorkloadName: brief.Resource.Name,
			WorkloadKind: investigation.WorkloadDeployment,
		},
		Window:     brief.Window,
		Controls:   investigation.DefaultControls(),
		Hypotheses: nil,
		KnownPods:  podNames(brief),
		// The opening pass, deliberately. The justification was already checked when the answer
		// was decoded — against the hypotheses the model was actually shown — and re-checking it
		// here would need those hypotheses threaded in only to refuse every read for lacking them.
		// What is being validated here is the READ: its namespace, its workload, its window and
		// its pod, which is the half that reaches a cluster.
		Pass: 0,
	}

	items := make([]investigation.Item, 0)
	refused := make([]string, 0)
	for _, proposal := range proposals {
		checked := proposal
		checked.Justification = 0
		admission := investigation.Admit(checked, bounds)
		if !admission.Admitted {
			refused = append(refused, fmt.Sprintf("%s: %s",
				proposal.CapabilityID, admission.Refusal))
			continue
		}
		items = append(items, evidenceFor(proposal)...)
	}
	return renumbered(items), refused
}

func podNames(brief investigation.Brief) []string {
	names := make([]string, 0, len(brief.Topology))
	for _, fact := range brief.Topology {
		names = append(names, fact.Pod)
	}
	return names
}

func renumbered(items []investigation.Item) []investigation.Item {
	for index := range items {
		items[index].Ordinal = index + 1
	}
	return items
}

func live(
	hypotheses []investigation.Hypothesis, settlings []investigation.Settling,
) []investigation.Hypothesis {
	states := make([]investigation.HypothesisState, len(hypotheses))
	for index, hypothesis := range hypotheses {
		states[index] = hypothesis.State
	}
	for _, settling := range settlings {
		if settling.Hypothesis >= 1 && settling.Hypothesis <= len(states) && settling.State != 0 {
			states[settling.Hypothesis-1] = settling.State
		}
	}
	remaining := make([]investigation.Hypothesis, 0, len(hypotheses))
	for index, state := range states {
		if state == investigation.HypothesisLive {
			remaining = append(remaining, hypotheses[index])
		}
	}
	return remaining
}

// observer collects every record the service publishes, which is where the attribution, the token
// breakdown and the cost come from.
type observer struct {
	mutex   sync.Mutex
	entries []reasoning.Record
}

func (o *observer) add(entry reasoning.Record) {
	o.mutex.Lock()
	defer o.mutex.Unlock()
	o.entries = append(o.entries, entry)
}

func (o *observer) all() []reasoning.Record {
	o.mutex.Lock()
	defer o.mutex.Unlock()
	return append([]reasoning.Record(nil), o.entries...)
}
