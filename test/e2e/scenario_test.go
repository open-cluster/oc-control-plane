package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

// The instrument itself needs tests — few, and about the parts that could silently lie.
//
// An instrument that reported a wrong number loudly would be repaired. One that reports a
// plausible number quietly corrupts the evidence being used to steer the product, and every test
// here is about one of the ways that could happen: a run of the wrong scenario scored as the
// right one, an answer shown to the person judging the reasoning, or an artifact missing the part
// a judgement rested on.
//
// These run in the ordinary suite because they need neither a cluster nor a model. What they
// cannot cover is whether the scenarios are REPRESENTATIVE. They are not, and cannot be made so;
// that is stated in the artifact rather than tested for.

func TestTheSetIsTenScenariosWithDistinctIdentities(t *testing.T) {
	scenarios := Scenarios()
	if len(scenarios) != 10 {
		t.Fatalf("the first set is ten scenarios, got %d", len(scenarios))
	}

	seen := map[string]bool{}
	for _, scenario := range scenarios {
		if seen[scenario.ID] {
			t.Errorf("%q is declared twice; a run filed under a shared identity cannot be "+
				"compared against its own history", scenario.ID)
		}
		seen[scenario.ID] = true

		switch {
		case scenario.Truth.Cause == "":
			t.Errorf("%s declares no cause; a scenario whose truth is not written down first "+
				"can be rationalised into correctness afterwards", scenario.ID)
		case len(scenario.Truth.Decisive) == 0:
			t.Errorf("%s names no decisive evidence, so evidence selection cannot be scored "+
				"against anything", scenario.ID)
		case scenario.Truth.Expected == 0:
			t.Errorf("%s says nothing about what a correct outcome looks like", scenario.ID)
		case scenario.Truth.Note == "":
			t.Errorf("%s does not say why it is in the set", scenario.ID)
		case scenario.Provision == nil || scenario.Ready == nil:
			t.Errorf("%s cannot be provisioned or cannot be verified", scenario.ID)
		}
	}
}

// The set must contain failures the system should NOT be able to explain. A set where every
// scenario is solvable measures ceiling and not honesty.
func TestTheSetContainsFailuresThatMustNotBeExplained(t *testing.T) {
	var abstentions, caveated int
	for _, scenario := range Scenarios() {
		switch scenario.Truth.Expected {
		case ExpectAbstention:
			abstentions++
		case ExpectCaveatedExplanation:
			caveated++
		}
	}
	if abstentions == 0 {
		t.Error("no scenario expects an abstention, so abstention is assumed rather than exercised")
	}
	if caveated == 0 {
		t.Error("no scenario expects a caveated answer, so a confident conclusion over " +
			"incomplete evidence is never caught")
	}
}

// Containment is exercised by the instrument rather than asserted by a document.
func TestOneScenarioCarriesAPromptInjectionAttempt(t *testing.T) {
	var carrying []string
	for _, scenario := range Scenarios() {
		if strings.Contains(renderProvisioning(t, scenario), injectionAttempt) {
			carrying = append(carrying, scenario.ID)
		}
	}
	if len(carrying) == 0 {
		t.Fatal("no scenario puts a prompt-injection attempt in a container's log output, so " +
			"containment is asserted rather than tested")
	}
}

// renderProvisioning runs a scenario's provisioning against a fake cluster and returns everything
// its containers would say.
//
// The context is short and its expiry is ignored on purpose: two scenarios wait for a pod to
// settle, which never happens against a fake, and what is being read here is what was SUBMITTED
// before the wait rather than what the cluster did with it.
func renderProvisioning(t *testing.T, scenario Scenario) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	client := fake.NewSimpleClientset()
	_ = scenario.Provision(ctx, client)

	sets, err := client.AppsV1().StatefulSets(scenario.Namespace).List(
		context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("%s: reading what provisioning created: %v", scenario.ID, err)
	}

	said := &strings.Builder{}
	for _, set := range sets.Items {
		for _, container := range set.Spec.Template.Spec.Containers {
			said.WriteString(strings.Join(container.Command, " "))
			said.WriteString("\n")
		}
	}
	return said.String()
}

// A pod name has to be knowable before the cluster creates it, or every recorded transcript that
// proposes reading a log is single-use and no two runs of one scenario can be compared.
func TestEveryScenarioWorkloadHasAPredictablePodName(t *testing.T) {
	for _, scenario := range Scenarios() {
		if scenario.Kind != "statefulset" {
			t.Errorf("%s is a %s, whose pod name the cluster generates; a recorded transcript "+
				"naming that pod would be valid for exactly one run", scenario.ID, scenario.Kind)
		}
		if podOf(scenario.Workload) != scenario.Workload+"-0" {
			t.Errorf("%s: the pod name is not derivable from the workload", scenario.ID)
		}
	}
}

// A cluster that did not reach its declared broken state is DISCARDED. Scoring a run of a
// different failure than the one declared is the worst thing this instrument could do.
func TestAClusterThatDidNotReachItsStateIsDiscarded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	client := fake.NewSimpleClientset()
	scenario := Scenario{
		ID: "never-ready", Namespace: "somewhere", Workload: "thing", Kind: "statefulset",
		Provision: func(context.Context, kubernetes.Interface) error { return nil },
		Ready: func(readyCtx context.Context, _ kubernetes.Interface) error {
			<-readyCtx.Done()
			return errors.New("the container never entered the declared state")
		},
	}

	err := scenario.Prepare(ctx, client)
	if !errors.Is(err, ErrNotProvisioned) {
		t.Fatalf("a cluster that never reached its state must be discarded, got %v", err)
	}
	if !strings.Contains(err.Error(), "never-ready") {
		t.Errorf("the discard does not name the scenario: %v", err)
	}
}

// The other half: a readiness condition that HOLDS lets the run proceed. Without this the test
// above would pass against an implementation that discarded everything.
func TestAClusterThatReachedItsStateIsNotDiscarded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "thing-0", Namespace: "somewhere", Labels: map[string]string{"app": "thing"},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: scenarioContainer,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason: "ImagePullBackOff",
				}},
			}},
		},
	})

	scenario := Scenario{
		ID: "pull-failed", Namespace: "somewhere", Workload: "thing", Kind: "statefulset",
		Provision: func(context.Context, kubernetes.Interface) error { return nil },
		Ready: func(readyCtx context.Context, ready kubernetes.Interface) error {
			return awaitPod(readyCtx, ready, "somewhere", selectorFor("thing"),
				"the image pull to fail", func(pod *corev1.Pod) bool {
					return waitingReason(pod) == "ImagePullBackOff"
				})
		},
	}

	if err := scenario.Prepare(ctx, client); err != nil {
		t.Fatalf("a cluster that DID reach its declared state was discarded: %v", err)
	}
}

// Ground truth is never present in the artifact handed to a scorer. This is the test that would
// catch the mistake nobody would notice until the scores were already worthless.
func TestGroundTruthNeverAppearsInTheArtifact(t *testing.T) {
	filed, err := NewResults(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	for _, scenario := range Scenarios() {
		const runID = "11111111-2222-3333-4444-555555555555"

		truth := GroundTruthOf(scenario, runID, time.Now())
		if _, err = filed.WriteGroundTruth(truth); err != nil {
			t.Fatal(err)
		}
		path, err := filed.WriteArtifact(Artifact{
			RunID:      runID,
			Components: Components{CodeVersion: "test"},
			// A case file carrying everything a real one would, including the workload's own name
			// and namespace, which DO legitimately appear.
			CaseFile: json.RawMessage(`{"investigation":{"scope":{"namespace":"` +
				scenario.Namespace + `","workloadName":"` + scenario.Workload + `"}}}`),
		})
		if err != nil {
			t.Fatal(err)
		}

		written, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		artifact := string(written)

		for _, secret := range append([]string{
			scenario.ID, scenario.Title, scenario.Truth.Cause, scenario.Truth.Note,
			scenario.Truth.Expected.String(),
		}, scenario.Truth.Decisive...) {
			if secret == "" {
				continue
			}
			if strings.Contains(artifact, secret) {
				t.Errorf("%s: the artifact a scorer reads carries %q. A scorer who knows the "+
					"answer grades recognition rather than reasoning.", scenario.ID, secret)
			}
		}
	}
}

// The artifact is complete: what the product assembled is what the scorer reads, unaltered.
//
// Completeness of the ASSEMBLY against storage is proven in the control plane's own suite, where
// there is a database to compare against. What can only go wrong here is the harness losing part
// of it on the way to a file, so that is what this asserts.
func TestTheArtifactCarriesTheWholeAssembledCase(t *testing.T) {
	filed, err := NewResults(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// Every section the product assembles, each with something in it that a judgement would rest
	// on: a request with its justification, an item, a gap, a hypothesis, an outcome.
	assembled := `{
	  "investigation": {"id": "case-1"},
	  "caseVersion": 4,
	  "rounds": [{"id": "round-1", "brief": {"resource": {"kind": "statefulset"}}}],
	  "hypotheses": [{"id": "hyp-1", "statement": "the limit was lowered"}],
	  "stances": [{"hypothesisId": "hyp-1", "evidenceId": "ev-1", "stance": "supports"}],
	  "evidence": [{"id": "ev-1", "statement": "the container was OOMKilled"}],
	  "timeline": [{"id": "ev-1"}],
	  "coverageGaps": [{"cause": "source cannot answer for the whole window"}],
	  "activity": [{"id": "req-1", "reason": "to see why it died",
	                "justifyingHypothesisId": "hyp-1"}],
	  "coverage": [{"capabilityId": "kubernetes.container.logs", "state": "checked"}],
	  "outcomes": [{"kind": "explanation", "statement": "the memory limit was lowered"}]
	}`

	path, err := filed.WriteArtifact(Artifact{
		RunID: "run-1", CaseFile: json.RawMessage(assembled),
		Cost: Cost{Tokens: 1200, MicroCents: 340, Requests: 5}, Elapsed: 92 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	var read Artifact
	if err = readJSON(path, &read); err != nil {
		t.Fatal(err)
	}

	var sections map[string]json.RawMessage
	if err = json.Unmarshal(read.CaseFile, &sections); err != nil {
		t.Fatalf("the artifact's case file is not readable: %v", err)
	}
	for _, section := range []string{
		"rounds", "hypotheses", "stances", "evidence", "timeline",
		"coverageGaps", "activity", "coverage", "outcomes",
	} {
		if len(sections[section]) == 0 {
			t.Errorf("the artifact lost %q; a judgement resting on it would rest on nothing",
				section)
		}
	}
	if !strings.Contains(string(sections["activity"]), "justifyingHypothesisId") {
		t.Error("a request reached the artifact without its justification, so the scorer cannot " +
			"judge the investigation rather than only its answer")
	}

	// The two numbers the buyer asks about survive as well.
	if read.Cost.Tokens != 1200 || read.Cost.MicroCents != 340 {
		t.Errorf("the run's cost was lost: %+v", read.Cost)
	}
	if read.ElapsedHuman == "" {
		t.Error("the run's wall-clock time was lost, so nobody can tell whether waiting beats " +
			"typing")
	}
	if len(read.Assumptions) == 0 {
		t.Error("the artifact does not state what this instrument cannot prove about itself")
	}
}

// A run given nothing to reason with is refused by name, rather than falling back to something and
// being scored as though a model had answered.
func TestARunWithNoModelSourceIsRefusedRatherThanQuietlyFallingBack(t *testing.T) {
	_, err := transcriptFor(Scenarios()[0], ModelSource{})
	if !errors.Is(err, ErrNoModelSource) {
		t.Fatalf("a run with no model source must be refused, got %v", err)
	}

	path, err := transcriptFor(Scenarios()[0], ModelSource{TranscriptDir: "recordings"})
	if err != nil {
		t.Fatalf("a recorded run must resolve: %v", err)
	}
	if want := filepath.Join("recordings", Scenarios()[0].ID+".json"); path != want {
		t.Errorf("transcript path = %q, want %q", path, want)
	}
}

// A live deployment needs no recording, and outranks one it was given alongside: a run that named
// a provider was asked for the real thing, and replaying at it would answer a different question.
func TestALiveProviderNeedsNoRecordingAndOutranksOne(t *testing.T) {
	live := ModelSource{Provider: "zai", Model: "glm-5", KeyFile: "/tmp/zai.key"}
	if !live.Live() {
		t.Fatal("a fully named deployment does not read as live")
	}

	path, err := transcriptFor(Scenarios()[0], live)
	if err != nil {
		t.Fatalf("a live run must resolve without a recording: %v", err)
	}
	if path != "" {
		t.Errorf("a live run resolved recording %q; it should need none", path)
	}

	both := live
	both.TranscriptDir = "recordings"
	if path, err = transcriptFor(Scenarios()[0], both); err != nil || path != "" {
		t.Errorf("a recording outranked the live provider: path %q, err %v", path, err)
	}

	// The artifact has to say which model answered, so a scorer is never guessing.
	if got := live.Describe(); got != "live provider zai/glm-5" {
		t.Errorf("the artifact would describe the source as %q", got)
	}
}

// A partially named deployment is not live. It would otherwise reach the control plane missing a
// model or a credential and fail there, hours of provisioning later.
func TestAPartiallyNamedDeploymentIsNotLive(t *testing.T) {
	for name, source := range map[string]ModelSource{
		"no model":      {Provider: "zai", KeyFile: "/tmp/k"},
		"no credential": {Provider: "zai", Model: "glm-5"},
		"no provider":   {Model: "glm-5", KeyFile: "/tmp/k"},
	} {
		if source.Live() {
			t.Errorf("%s reads as a live deployment", name)
		}
	}
}
