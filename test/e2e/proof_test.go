package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"
)

// reconnectTimeout bounds a job dispatched after a control-plane restart. It is longer than
// an ordinary job because the Relay's reconnect backoff is capped at thirty seconds with full
// jitter, and a restart usually costs it at least one failed attempt.
const reconnectTimeout = 3 * time.Minute

// observationWindow is how long the harness watches for something that must not happen. It is
// longer than the control plane's delivery round, so work that was going to be dispatched has
// had its chance.
const observationWindow = 12 * time.Second

// The proof, as one run. The subtests are phases of a single life rather than independent
// cases: they share a Relay that enrols once, a cluster that is created once, and a control
// plane whose restart in the last phase is the thing being tested. Splitting them into
// separate top-level tests would multiply several minutes of containers by the number of
// phases and prove nothing extra.
//
// The order is not incidental. Each phase leaves the harness usable by the next, and the two
// that disturb it — stopping the Relay, killing the control plane — come last and restore
// what they took.
func TestTheProtocolCarriesWorkBetweenRealProcesses(t *testing.T) {
	h := newHarness(t)

	t.Run("a job crosses the wire, executes, and its result is recorded", func(t *testing.T) {
		job := h.dispatch(t)
		record := h.awaitTerminal(t, job)

		if record.Status != jobSucceeded {
			t.Fatalf("the job ended %s, want succeeded: %s\n\n%s",
				record.Status, describeFailure(record), h.diagnostics())
		}
		if record.LeaseEpoch < 1 {
			t.Errorf("a recorded job must carry the generation of the lease it ran under, got %d",
				record.LeaseEpoch)
		}

		result := decodeResult(t, record.Result)
		assertReadTheFixture(t, result)
		assertCompletenessBasisIntact(t, result)
	})

	// A read that established the workload is not there is a successful investigation, not a
	// failed one, and the difference is load-bearing: certified absence claims are minted
	// centrally from typed read outcomes, and a not-found is never absence evidence. If the
	// transport collapsed the two — recording a failure, or a success whose completeness flag
	// defaulted true — the platform would either lose the finding or certify a fact it never
	// established.
	t.Run("an absent workload is a typed outcome, not a failure", func(t *testing.T) {
		job := h.dispatchRead(t, fixtureNamespace, "no-such-workload")
		record := h.awaitTerminal(t, job)

		if record.Status != jobSucceeded {
			t.Fatalf("reading an absent workload ended %s; establishing that something is not "+
				"there is a result, not a failure: %s", record.Status, describeFailure(record))
		}

		result := decodeResult(t, record.Result)
		want := relayv1.KubernetesReadOutcome_KUBERNETES_READ_OUTCOME_WORKLOAD_NOT_FOUND
		if result.GetOutcome() != want {
			t.Errorf("the read outcome is %s, want %s", result.GetOutcome(), want)
		}
		if result.GetComplete() {
			t.Error("no pod list was taken, so nothing may claim the read was complete")
		}
		if result.GetWorkload() != nil {
			t.Error("a workload summary is present only on success")
		}
	})

	// The contract says a Relay selects its implementation by version, and gives it a typed
	// way to say it cannot: KIND_UNSUPPORTED_CAPABILITY_VERSION. Both halves have to agree,
	// and only a real pair can show whether they do.
	//
	// What is at stake is not this version. It is the day a v2 exists: an old Relay that
	// ignores the version executes the job with whatever implementation it has and returns a
	// result under the wrong semantics, and nothing downstream can tell. Rejecting the
	// assignment is what makes "this Relay is too old" a fact the control plane learns rather
	// than a difference it never sees.
	t.Run("a capability version the relay does not have is refused, not executed", func(t *testing.T) {
		job := h.dispatchVersion(t, 99, fixtureNamespace, fixtureWorkload)
		record := h.awaitTerminal(t, job)

		if record.Status != jobFailed {
			t.Fatalf("a job at an unsupported capability version ended %s; the relay ran "+
				"something it never claimed to support", record.Status)
		}
		var failure relayv1.JobFailure
		if err := proto.Unmarshal(record.Result, &failure); err != nil {
			t.Fatalf("the recorded failure could not be read: %v", err)
		}
		if failure.GetKind() != relayv1.JobFailure_KIND_UNSUPPORTED_CAPABILITY_VERSION {
			t.Errorf("the refusal is %s, want KIND_UNSUPPORTED_CAPABILITY_VERSION; the kind is "+
				"what says the relay is too old rather than the dispatch malformed",
				failure.GetKind())
		}
	})

	t.Run("a spent bootstrap token mints no second identity", h.assertTokenIsSpent)

	t.Run("work enqueued while no relay is connected is delivered on connect", func(t *testing.T) {
		h.relay.stop()
		// Whatever happens below, the next subtest must not inherit a stopped Relay. Restarting
		// one that is already running is harmless; leaving the rest of the run against a dead
		// one would turn one failure into four.
		t.Cleanup(func() { _ = h.relay.start() })

		job := h.dispatch(t)

		// Waiting is the assertion. There is no way to observe that something did not happen
		// except by giving it time to, and the window is longer than the round on which the
		// control plane claims work — so a job that was going to be delivered to a relay that
		// is not there would have been by now. Reading immediately after the insert would prove
		// only that a database write outruns a network round trip.
		time.Sleep(observationWindow)
		record, err := h.truth.job(context.Background(), organization, job)
		if err != nil {
			t.Fatalf("reading the job enqueued with no relay connected: %v", err)
		}
		if record.Status.terminal() {
			t.Fatalf("a job reached %s with no relay running; nothing executed it", record.Status)
		}

		// The same credential file, so this start loads the durable identity rather than
		// enrolling again — which is the half of identity a fresh enrolment never tests.
		if err = h.relay.start(); err != nil {
			t.Fatalf("restarting the relay: %v", err)
		}
		if final := h.awaitTerminal(t, job); final.Status != jobSucceeded {
			t.Fatalf("work waiting for a relay ended %s, want succeeded: %s\n\n%s",
				final.Status, describeFailure(final), h.diagnostics())
		}
	})

	t.Run("the relay recovers when the control plane dies", func(t *testing.T) {
		if err := h.plane.restart(context.Background()); err != nil {
			t.Fatalf("restarting the control plane: %v", err)
		}

		job := h.dispatch(t)
		record := h.awaitTerminalWithin(t, job, reconnectTimeout)
		if record.Status != jobSucceeded {
			t.Fatalf("after a control-plane restart the job ended %s, want succeeded: %s\n\n%s",
				record.Status, describeFailure(record), h.diagnostics())
		}
		// Only what the process running now has said counts. Searching the whole history
		// would find the session its predecessor accepted and call that proof, which would
		// leave this passing against a control plane that never went away.
		if !strings.Contains(h.plane.logsSinceStart(), "relay session established") {
			t.Errorf("the restarted control plane never accepted a session\n%s", h.plane.logs())
		}
	})
}

// assertTokenIsSpent runs a second Relay with the token the first one consumed.
//
// A fresh credential file is the point: this Relay has no identity, so it will attempt
// enrolment, and the only thing standing between it and one is the token having been spent
// once already. That guard is proven here against a real client rather than a modelled one.
func (h *harness) assertTokenIsSpent(t *testing.T) {
	before, err := h.truth.countRegistrations(context.Background(), organization)
	if err != nil {
		t.Fatalf("counting registrations: %v", err)
	}

	impostor, err := newRelay(h.installation("impostor", h.token))
	if err != nil {
		t.Fatalf("preparing the second relay: %v", err)
	}
	t.Cleanup(impostor.stop)

	if err = impostor.start(); err != nil {
		t.Fatalf("starting the second relay: %v", err)
	}
	if waitErr := impostor.program.wait(time.Minute); waitErr == nil {
		t.Errorf("a relay presenting a spent token ran to a clean exit\n%s", impostor.logs())
	}

	enrolled, err := impostor.enrolled()
	if err != nil {
		t.Fatalf("reading whether the second relay enrolled: %v", err)
	}
	if enrolled {
		t.Errorf("a relay presenting a spent token persisted a credential\n%s", impostor.logs())
	}

	// The control plane must have refused it. Everything above is also true of a Relay that
	// fell over before it ever dialled — a bad path, an unreadable kubeconfig — so without
	// this the test would pass while proving nothing about single-use consumption.
	//
	// The audit line is the observable here because a refusal deliberately writes nothing
	// else: no token is spent, no identity is minted, and telling an unknown token from a
	// spent one in the response is exactly what would let an attacker probe for valid ones.
	// It is read from the process running now rather than the whole history, for the same
	// reason the reconnect assertion is.
	h.await(t, "the control plane to record a refused enrolment", 30*time.Second,
		func(context.Context) (bool, error) {
			return strings.Contains(h.plane.logsSinceStart(), "relay enrolment refused"), nil
		})

	after, err := h.truth.countRegistrations(context.Background(), organization)
	if err != nil {
		t.Fatalf("counting registrations: %v", err)
	}
	if after != before {
		t.Errorf("registrations went from %d to %d; a spent token minted an identity", before, after)
	}
}

// assertReadTheFixture checks the result describes the workload that was created, which is
// what proves the job reached a real cluster rather than any cluster.
func assertReadTheFixture(t *testing.T, result *relayv1.KubernetesWorkloadRuntimeResultV1) {
	t.Helper()

	if result.GetOutcome() != relayv1.KubernetesReadOutcome_KUBERNETES_READ_OUTCOME_SUCCESS {
		t.Fatalf("the read outcome is %s, want SUCCESS", result.GetOutcome())
	}

	workload := result.GetWorkload()
	if workload.GetName() != fixtureWorkload || workload.GetNamespace() != fixtureNamespace {
		t.Errorf("the result describes %s/%s, want %s/%s",
			workload.GetNamespace(), workload.GetName(), fixtureNamespace, fixtureWorkload)
	}
	if workload.GetKind() != "deployment" {
		t.Errorf("workload kind = %q, want %q", workload.GetKind(), "deployment")
	}
	if workload.GetDesiredReplicas() != 1 || workload.GetReadyReplicas() != 1 {
		t.Errorf("replicas: desired %d ready %d, want 1 and 1",
			workload.GetDesiredReplicas(), workload.GetReadyReplicas())
	}
	// Empty exactly when the outcome is SELECTOR_UNREPRESENTABLE, which this is not.
	if workload.GetSelectorSummary() == "" {
		t.Error("a successful read must render the workload's selector")
	}

	if len(result.GetPods()) == 0 {
		t.Fatal("the fixture has a ready replica, so the read must report at least one pod")
	}
	pod := result.GetPods()[0]
	if pod.GetPhase() != "Running" || !pod.GetReady() {
		t.Errorf("pod %s is phase %q ready %t, want Running and ready",
			pod.GetName(), pod.GetPhase(), pod.GetReady())
	}
	if len(pod.GetContainers()) == 0 {
		t.Error("a pod's runtime must carry its containers")
	}
}

// assertCompletenessBasisIntact checks the fields the central certificate logic depends on
// arrived and are typed as expected.
//
// This is the assertion with the least margin for politeness. A certified absence claim is
// minted centrally and only from a complete read, so a transport that quietly dropped
// `complete` would leave every claim resting on a default false — or, worse the other way
// round, a default true would let an incomplete read certify an absence that is not a fact.
func assertCompletenessBasisIntact(t *testing.T, result *relayv1.KubernetesWorkloadRuntimeResultV1) {
	t.Helper()

	if !result.GetComplete() {
		t.Error("the single bounded pod list returned no continuation token, " +
			"so the read is complete and must say so")
	}
	if got, want := result.GetReturnedPodCount(), int32(len(result.GetPods())); got != want {
		t.Errorf("returned pod count = %d, but %d pods arrived", got, want)
	}
	// The dispatch asked for ten and the Relay's local cap is higher, so ten is what applies.
	if result.GetAppliedMaxPods() != 10 {
		t.Errorf("applied max pods = %d, want the dispatched bound of 10",
			result.GetAppliedMaxPods())
	}
	if result.GetReadAt() == nil {
		t.Error("the result must carry the time the source was read")
	}
}

func decodeResult(t *testing.T, recorded []byte) *relayv1.KubernetesWorkloadRuntimeResultV1 {
	t.Helper()

	if len(recorded) == 0 {
		t.Fatal("a succeeded job recorded no result payload")
	}
	var capability relayv1.CapabilityResult
	if err := proto.Unmarshal(recorded, &capability); err != nil {
		t.Fatalf("the recorded result is not a capability result: %v", err)
	}
	result := capability.GetKubernetesWorkloadRuntimeV1()
	if result == nil {
		t.Fatal("the recorded result carries no kubernetes.workload.runtime payload")
	}
	return result
}

// describeFailure renders a non-success outcome for a failure message. A job that failed
// carries a typed reason, and printing it is the difference between "the path is broken" and
// "the capability refused the arguments".
func describeFailure(record jobRecord) string {
	if record.Status == jobCancelled {
		return "the job was cancelled, which records no payload"
	}
	if record.Status != jobFailed || len(record.Result) == 0 {
		return "no failure payload was recorded"
	}
	var failure relayv1.JobFailure
	if err := proto.Unmarshal(record.Result, &failure); err != nil {
		return "the recorded failure payload could not be read"
	}
	return "failure kind " + failure.GetKind().String() + ": " + failure.GetDetail()
}
