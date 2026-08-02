// Command scenario is the scenario harness: a fixed set of Kubernetes clusters broken in known
// ways, on purpose, with the cause written down before the system ever sees them.
//
// It exists because there is otherwise no way to know whether the product works. An investigation
// produces an explanation, and nothing determines whether that explanation is right, whether an
// engineer would have been faster without it, or whether the system chose sensible evidence on the
// way there. The existing suites answer a different question — whether the machinery behaves as
// specified — and the system can pass every one of them while producing explanations no engineer
// would act on.
//
// It is a PROGRAM rather than a test, deliberately. It has a human in the loop, it calls a paid
// model, and it produces a judgement rather than a pass. Under `go test` it would either weaken CI
// or misrepresent what it proves. It is never a commit gate: a gate that fails on ordinary model
// variance is ignored within two weeks, and this is too valuable to become background noise.
//
// # Running it
//
//	scenario run    -results ./harness-run -transcripts ./transcripts
//	scenario run    -results ./harness-run -transcripts ./transcripts -scenario red-herring
//	scenario score  -results ./harness-run
//	scenario list
//
// `run` provisions each scenario, verifies the cluster actually reached its declared broken state,
// investigates it through the real control plane and a real Relay, and files two things apart: an
// artifact a scorer reads, and the ground truth they must not. `score` joins them AFTER the
// scoring is recorded and applies the kill criterion.
//
// It needs a container runtime, this repository, and the Relay's working tree beside it (or named
// by OC_E2E_RELAY_SOURCE).
//
// # Cadence
//
// Manually, before a release, and periodically. Model drift is a first-class reason to run it: the
// same set against a new provider version is the only way to learn that an upgrade degraded
// investigations, and it is the specific failure a customer would otherwise report first.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/open-cluster/oc-control-plane/test/e2e"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "\n%v\n", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		usage()
		return errors.New("no command given")
	}

	switch os.Args[1] {
	case "run":
		return runSet(os.Args[2:])
	case "score":
		return scoreSet(os.Args[2:])
	case "list":
		return list()
	default:
		usage()
		return fmt.Errorf("unknown command %q", os.Args[1])
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `the scenario harness

  scenario run   -results DIR [-transcripts DIR] [-scenario ID] [-version V]
  scenario score -results DIR
  scenario list

run    provision each scenario, verify it reached its declared broken state, investigate it,
       and file the artifact and the ground truth apart
score  join recorded scores with ground truth and apply the kill criterion
list   the scenarios in the set
`)
}

func runSet(arguments []string) error {
	flags := flag.NewFlagSet("run", flag.ExitOnError)
	results := flags.String("results", "", "directory to file artifacts, ground truth and scores in")
	transcripts := flags.String("transcripts", "",
		"directory of recorded model transcripts, one per scenario, named <scenario-id>.json")
	only := flags.String("scenario", "",
		"run one scenario by identifier, so iterating on the planner does not require the whole set")
	version := flags.String("version", "dev", "the build under evaluation, recorded with every run")

	// The live deployment. A run given one calls a real model and records the transcript as a
	// by-product; a run given a transcripts directory instead replays. Given both, the live
	// provider wins, because a run that named one was asked for the real thing.
	provider := flags.String("provider", "", "live model provider: anthropic or zai")
	model := flags.String("model", "", "the exact model identifier the provider serves")
	keyFile := flags.String("key-file", "",
		"path to a file holding the provider credential; never the credential itself")
	effort := flags.String("effort", "", "how hard to think: low, medium, high, xhigh, max")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *results == "" {
		return errors.New("-results is required: a run that files nothing proves nothing")
	}

	filed, err := e2e.NewResults(*results)
	if err != nil {
		return err
	}

	scenarios := e2e.Scenarios()
	if *only != "" {
		scenario, found := e2e.ScenarioByID(*only)
		if !found {
			return fmt.Errorf("no scenario is called %q; `scenario list` names them all", *only)
		}
		scenarios = []e2e.Scenario{scenario}
	}

	// SIGINT stops the run between steps rather than leaving containers behind: each run owns a
	// database and a cluster, and a harness that leaked them would cost more to use than it is
	// worth.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err = e2e.RunSet(ctx, scenarios, e2e.Options{
		Results: filed,
		Model: e2e.ModelSource{
			TranscriptDir: *transcripts,
			Provider:      *provider,
			Model:         *model,
			KeyFile:       *keyFile,
			Effort:        *effort,
		},
		CodeVersion: *version,
		Progress:    func(format string, args ...any) { fmt.Printf(format+"\n", args...) },
	})
	if err != nil {
		return err
	}

	fmt.Printf("\nartifacts are in %s\n", filed.ArtifactDir())
	fmt.Print("Give that directory — and nothing beside it — to two engineers who did not build " +
		"the system.\nThey score blind; ground truth is joined afterwards by `scenario score`.\n")
	return nil
}

func scoreSet(arguments []string) error {
	flags := flag.NewFlagSet("score", flag.ExitOnError)
	results := flags.String("results", "", "the directory a run filed into")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *results == "" {
		return errors.New("-results is required")
	}

	filed, err := e2e.NewResults(*results)
	if err != nil {
		return err
	}
	truth, err := filed.ReadGroundTruth()
	if err != nil {
		return err
	}
	scores, err := filed.ReadScores()
	if err != nil {
		return err
	}

	result := e2e.JoinScores(truth, scores)
	path, err := filed.WriteSetResult(result)
	if err != nil {
		return err
	}

	report(result)
	fmt.Printf("\n%s\n", path)
	if !result.Passed {
		return errors.New("the set did not pass")
	}
	return nil
}

// report says what the set result means in the terms the criterion is stated in, rather than as a
// number. There is no percentage here on purpose: one wrong-and-confident answer fails the set,
// and a score that could be averaged is one that would be.
func report(result e2e.SetResult) {
	fmt.Printf("\n%d run(s) scored\n", len(result.Runs))

	for _, refused := range result.Refused {
		fmt.Printf("  REFUSED SCORE: %s\n", refused)
	}
	for _, run := range result.Runs {
		if run.Unscored != "" {
			fmt.Printf("  %-28s %s\n", run.ScenarioID, run.Unscored)
			continue
		}
		for _, score := range run.Scores {
			fmt.Printf("  %-28s %-24s %s\n", run.ScenarioID, score.Conclusion, score.Scorer)
		}
		if run.Disagreement != "" {
			fmt.Printf("  %-28s DISAGREEMENT: %s\n", "", run.Disagreement)
		}
	}

	fmt.Printf("\n%d run(s) at least one scorer said would have saved them time\n",
		result.TimeSaved)

	switch {
	case len(result.FailedBecause) > 0:
		fmt.Print("\nTHE SET FAILED. A wrong and confident answer is the kill criterion: one " +
			"occurrence\nfails the set, and it is not averaged, weighted or traded against " +
			"successes elsewhere.\n")
		for _, because := range result.FailedBecause {
			fmt.Printf("  %s\n", because)
		}
	case len(result.Incomplete) > 0:
		fmt.Printf("\nTHE SET IS INCOMPLETE: %d run(s) carry no valid score. A set is not passed "+
			"by runs\nnobody judged.\n", len(result.Incomplete))
	default:
		fmt.Print("\nThe set passed: no run was judged wrong and confident.\n")
	}
}

func list() error {
	for _, scenario := range e2e.Scenarios() {
		fmt.Printf("%-28s %s\n", scenario.ID, scenario.Title)
	}
	return nil
}
