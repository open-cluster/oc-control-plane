package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"
)

const reconnectTimeout = 3 * time.Minute

const observationWindow = 12 * time.Second

func TestTheProtocolCarriesWorkBetweenRealProcesses(t *testing.T) {
	h := newHarness(t)

	t.Run("an investigation reads a real cluster and cites its relay result", h.assertInvestigation)

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
	t.Run("in-flight work honors cancellation fencing and reconnect recovery", h.assertInFlightGuarantees)
	t.Run("a lost result acknowledgement causes one safe idempotent resend", h.assertIdempotentResultResend)
	t.Run("the real relay refuses namespaces excluded by its local allowlist", h.assertRelayNamespaceBoundary)

	t.Run("work enqueued while no relay is connected is delivered on connect", func(t *testing.T) {
		h.relay.stop()
		t.Cleanup(func() { _ = h.relay.start() })

		job := h.dispatch(t)

		time.Sleep(observationWindow)
		record, err := h.truth.job(context.Background(), organization, job)
		if err != nil {
			t.Fatalf("reading the job enqueued with no relay connected: %v", err)
		}
		if record.Status.terminal() {
			t.Fatalf("a job reached %s with no relay running; nothing executed it", record.Status)
		}

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
		if !strings.Contains(h.plane.logsSinceStart(), "relay session established") {
			t.Errorf("the restarted control plane never accepted a session\n%s", h.plane.logs())
		}
	})
}

func (h *harness) assertRelayNamespaceBoundary(t *testing.T) {
	t.Helper()

	h.relay.stop()
	h.relay.environment["RELAY_ALLOWED_NAMESPACES"] = fixtureNamespace
	restore := func() {
		h.relay.stop()
		delete(h.relay.environment, "RELAY_ALLOWED_NAMESPACES")
		if err := h.relay.start(); err != nil {
			t.Errorf("restoring the Relay after its namespace policy check: %v", err)
		}
	}
	t.Cleanup(restore)
	if err := h.relay.start(); err != nil {
		t.Fatalf("starting the Relay with a customer-owned namespace allowlist: %v", err)
	}

	denied := h.awaitTerminal(t, h.dispatchRead(t, "outside-customer-boundary", fixtureWorkload))
	if denied.Status != jobFailed {
		t.Fatalf("an excluded namespace ended %s; the Relay must enforce customer-owned policy", denied.Status)
	}
	var failure relayv1.JobFailure
	if err := proto.Unmarshal(denied.Result, &failure); err != nil {
		t.Fatalf("decoding the Relay's namespace refusal: %v", err)
	}
	if failure.GetKind() != relayv1.JobFailure_KIND_LOCAL_POLICY_REFUSED {
		t.Fatalf("the excluded namespace produced %s, want KIND_LOCAL_POLICY_REFUSED", failure.GetKind())
	}
	allowed := h.awaitTerminal(t, h.dispatchRead(t, fixtureNamespace, fixtureWorkload))
	if allowed.Status != jobSucceeded {
		t.Fatalf("a permitted namespace ended %s: %s", allowed.Status, describeFailure(allowed))
	}
}

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

func assertCompletenessBasisIntact(t *testing.T, result *relayv1.KubernetesWorkloadRuntimeResultV1) {
	t.Helper()

	if !result.GetComplete() {
		t.Error("the single bounded pod list returned no continuation token, " +
			"so the read is complete and must say so")
	}
	if got, want := result.GetReturnedPodCount(), int32(len(result.GetPods())); got != want {
		t.Errorf("returned pod count = %d, but %d pods arrived", got, want)
	}
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
