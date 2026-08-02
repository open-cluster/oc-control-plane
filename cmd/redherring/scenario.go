package main

import (
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/capability"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

// THE RED HERRING, WRITTEN DOWN BEFORE THE MODEL SEES IT.
//
// The cause is a rotated database credential. The distractor is a deployment image update thirty
// minutes earlier, which is the obvious thing to blame and is what a change-aware investigator is
// most likely to be led by.
//
// Three facts discriminate between them, and all three are in the evidence rather than in the
// prompt's framing:
//
//   - the rollout completed and the pods were Ready for half an hour afterwards;
//   - the pod still running the PREVIOUS image is failing in exactly the same way;
//   - the container's own log says authentication was refused, which an image change does not
//     cause.
//
// A good answer reaches the credential and says why the deploy is not it. An answer that blames the
// deploy has been led by proximity. An abstention is over-cautious here: the evidence does point
// somewhere, and saying so is the whole product.

// The incident's clock. Everything is relative to these so the timeline reads correctly.
var (
	windowStart = time.Date(2026, 8, 2, 11, 0, 0, 0, time.UTC)
	deployedAt  = time.Date(2026, 8, 2, 11, 15, 0, 0, time.UTC)
	healthyAt   = time.Date(2026, 8, 2, 11, 16, 0, 0, time.UTC)
	brokeAt     = time.Date(2026, 8, 2, 11, 47, 0, 0, time.UTC)
	windowEnd   = time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
)

var (
	changeEvidence   = uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000001")
	topologyEvidence = uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000002")
)

// redHerringBrief is the deterministic orientation, exactly as the runner would have assembled it.
func redHerringBrief() investigation.Brief {
	return investigation.Brief{
		Resource: investigation.ResourceIdentity{
			Kind:               "deployment",
			Name:               "checkout",
			Namespace:          "payments",
			UID:                "9f2c1c2e-5d1a-4a3b-9c77-0d5b7f2a4e10",
			DesiredReplicas:    3,
			ReadyReplicas:      0,
			UpdatedReplicas:    2,
			AvailableReplicas:  0,
			Generation:         12,
			ObservedGeneration: 12,
			ContainerImages:    []string{"registry.internal/checkout:2.8.1"},
			CreatedAt:          time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC),
			Resolved:           true,
		},
		Trigger: investigation.Trigger{Kind: investigation.TriggerManual},
		Window:  investigation.Window{Start: windowStart, End: windowEnd},
		RecentChanges: []investigation.Change{{
			At: deployedAt,
			Summary: "deployment image updated from registry.internal/checkout:2.8.0 to " +
				"registry.internal/checkout:2.8.1",
			Evidence: changeEvidence,
		}},
		Topology: []investigation.TopologyFact{
			{
				Pod: "checkout-6b4f9c7d8-2xk4m", Node: "ip-10-0-3-14", Owner: "checkout-6b4f9c7d8",
				Phase: "Running", Ready: false, Evidence: topologyEvidence,
			},
			{
				Pod: "checkout-6b4f9c7d8-9wq7p", Node: "ip-10-0-4-201", Owner: "checkout-6b4f9c7d8",
				Phase: "CrashLoopBackOff", Ready: false, Evidence: topologyEvidence,
			},
			{
				// The discriminating pod: it is still on the PREVIOUS ReplicaSet, so it never
				// took the new image, and it is failing in exactly the same way.
				Pod: "checkout-5d8c7b6a4-mn3vt", Node: "ip-10-0-2-88", Owner: "checkout-5d8c7b6a4",
				Phase: "CrashLoopBackOff", Ready: false, Evidence: topologyEvidence,
			},
		},
		Available: []investigation.CapabilityRef{
			{ID: capability.KubernetesWorkloadRuntime, Version: capability.SchemaVersion1},
			{ID: capability.KubernetesNamespaceEvents, Version: capability.SchemaVersion1},
			{ID: capability.KubernetesContainerLogs, Version: capability.SchemaVersion1},
		},
		Coverage: []investigation.Coverage{
			{
				CapabilityID:      capability.KubernetesWorkloadRuntime,
				CapabilityVersion: capability.SchemaVersion1,
				State:             investigation.CoverageChecked,
				Reason:            "the read returned and produced evidence",
				Evidence:          1,
			},
			{
				CapabilityID:      capability.KubernetesNamespaceEvents,
				CapabilityVersion: capability.SchemaVersion1,
				State:             investigation.CoverageChecked,
				Reason:            "the read returned and produced evidence",
				Evidence:          1,
			},
			{
				CapabilityID:      capability.KubernetesContainerLogs,
				CapabilityVersion: capability.SchemaVersion1,
				State:             investigation.CoverageUnavailable,
				Reason:            "no read of this capability was made in this round",
			},
		},
		AssembledAt: windowEnd,
	}
}

// redHerringGaps is what this round could not check. It is real rather than decorative: the
// cluster's event retention does not reach the whole window, which is exactly the kind of thing a
// caveated conclusion should name.
func redHerringGaps() []investigation.Gap {
	return []investigation.Gap{{
		ID:           uuid.MustParse("bbbbbbbb-0000-0000-0000-000000000001"),
		Ordinal:      1,
		Cause:        investigation.GapRetentionHorizon,
		CapabilityID: capability.KubernetesNamespaceEvents,
		Subject:      "the cluster's account of what it did before 11:12",
		Consequence: "events older than the cluster's retention cannot be weighed, so anything " +
			"that happened earlier in the window is outside what this round can see",
	}}
}

// evidenceFor answers one proposed read from the pre-baked set.
//
// The evidence is fixed rather than generated, so two runs of this scenario are comparable and a
// change in the answer is a change in the model or the prompt rather than in the input.
func evidenceFor(proposal investigation.Proposal) []investigation.Item {
	switch proposal.CapabilityID {
	case capability.KubernetesContainerLogs:
		return containerLog(proposal.Arguments.PodName, proposal.Arguments.Previous)
	case capability.KubernetesNamespaceEvents:
		return namespaceEvents()
	case capability.KubernetesWorkloadRuntime:
		return workloadRuntime()
	default:
		return nil
	}
}

// containerLog is the decisive artifact. Whichever pod is asked for, the log says the same thing:
// the database is refusing the credential. The pod on the old image says it too, which is what
// takes the deploy out of the running.
func containerLog(pod string, previous bool) []investigation.Item {
	which := "the current container instance"
	if previous {
		which = "the container instance before the current one"
	}
	return []investigation.Item{{
		ID:                uuid.New(),
		CapabilityID:      capability.KubernetesContainerLogs,
		CapabilityVersion: capability.SchemaVersion1,
		Connection:        uuid.MustParse("cccccccc-0000-0000-0000-000000000001"),
		Statement: "the checkout container in pod " + pod + " exits during startup after the " +
			"database refuses its credentials (" + which + ")",
		Content: `2026-08-02T11:47:03.118Z INFO  checkout starting, version 2.8.1
2026-08-02T11:47:03.204Z INFO  connecting to postgres at payments-db.payments.svc:5432
2026-08-02T11:47:03.377Z ERROR pq: password authentication failed for user "checkout_svc"
2026-08-02T11:47:03.377Z ERROR startup probe: cannot acquire database connection
2026-08-02T11:47:03.378Z FATAL exiting: database is required at startup`,
		Trust:            investigation.TrustRelayAttested,
		SourceObservedAt: brokeAt,
		ReceivedAt:       windowEnd,
	}}
}

// namespaceEvents is what the cluster itself said. The rollout succeeded and the pods were ready
// for half an hour before anything went wrong, which is the fact that breaks the correlation.
func namespaceEvents() []investigation.Item {
	connection := uuid.MustParse("cccccccc-0000-0000-0000-000000000001")
	return []investigation.Item{
		{
			ID:                uuid.New(),
			CapabilityID:      capability.KubernetesNamespaceEvents,
			CapabilityVersion: capability.SchemaVersion1,
			Connection:        connection,
			Statement: "the rollout to checkout:2.8.1 completed successfully and all three " +
				"replicas reported Ready",
			Content: "Normal  ScalingReplicaSet  Scaled up replica set checkout-6b4f9c7d8 to 3\n" +
				"Normal  Started            Started container checkout\n" +
				"Normal  Ready              Pod checkout-6b4f9c7d8-2xk4m is Ready",
			Trust:            investigation.TrustRelayAttested,
			SourceObservedAt: healthyAt,
			ReceivedAt:       windowEnd,
		},
		{
			ID:                uuid.New(),
			CapabilityID:      capability.KubernetesNamespaceEvents,
			CapabilityVersion: capability.SchemaVersion1,
			Connection:        connection,
			Statement: "readiness probes began failing across every checkout pod at 11:47, " +
				"including the pod still running the previous image",
			Content: "Warning  Unhealthy  Readiness probe failed: HTTP probe failed with " +
				"statuscode: 503\n" +
				"Warning  BackOff    Back-off restarting failed container checkout in pod " +
				"checkout-6b4f9c7d8-9wq7p\n" +
				"Warning  BackOff    Back-off restarting failed container checkout in pod " +
				"checkout-5d8c7b6a4-mn3vt",
			Trust:            investigation.TrustRelayAttested,
			SourceObservedAt: brokeAt,
			ReceivedAt:       windowEnd,
		},
	}
}

// workloadRuntime restates what the brief already established, which is what a real re-read of it
// would produce.
func workloadRuntime() []investigation.Item {
	return []investigation.Item{{
		ID:                uuid.New(),
		CapabilityID:      capability.KubernetesWorkloadRuntime,
		CapabilityVersion: capability.SchemaVersion1,
		Connection:        uuid.MustParse("cccccccc-0000-0000-0000-000000000001"),
		Statement: "checkout has 0 of 3 replicas ready; two pods run the new image and one runs " +
			"the previous one, and none of them is Ready",
		Content:          "",
		Trust:            investigation.TrustRelayAttested,
		SourceObservedAt: windowEnd,
		ReceivedAt:       windowEnd,
	}}
}
