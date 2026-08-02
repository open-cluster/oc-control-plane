package main

import (
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/capability"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

// This scenario costs money to run, so the parts of it that do not need a model are checked
// without one. A read refused for the wrong reason, or evidence that never arrives, would be
// discovered halfway through a paid run and would invalidate it.

// The reads this scenario has to be able to serve are the ones the schema lets a model ask for.
// If validation refuses them the run produces an investigation with no evidence, and the model is
// scored on a question nobody meant to ask.
func TestServe_AdmitsTheReadsAModelCanActuallyPropose(t *testing.T) {
	brief := redHerringBrief()
	pod := brief.Topology[2].Pod // the pod still on the previous image

	proposals := []investigation.Proposal{
		{
			CapabilityID:      capability.KubernetesContainerLogs,
			CapabilityVersion: capability.SchemaVersion1,
			Justification:     1,
			Reason:            "the previous instance's log would say why it exited",
			Arguments: investigation.Arguments{
				Namespace:     brief.Resource.Namespace,
				WorkloadKind:  investigation.WorkloadDeployment,
				WorkloadName:  brief.Resource.Name,
				PodName:       pod,
				ContainerName: "checkout",
				Previous:      true,
				Window:        brief.Window,
				MaxLines:      capability.MaxLines,
			},
		},
		{
			CapabilityID:      capability.KubernetesNamespaceEvents,
			CapabilityVersion: capability.SchemaVersion1,
			Justification:     2,
			Reason:            "what the cluster said around the failure",
			Arguments: investigation.Arguments{
				Namespace:    brief.Resource.Namespace,
				WorkloadKind: investigation.WorkloadDeployment,
				WorkloadName: brief.Resource.Name,
				Window:       brief.Window,
				MaxEvents:    capability.MaxEvents,
			},
		},
	}

	served, refused := serve(proposals, brief)

	if len(refused) != 0 {
		t.Fatalf("reads a model can legitimately propose were refused: %v", refused)
	}
	if len(served) == 0 {
		t.Fatal("no evidence was served, so the model would conclude from nothing")
	}
	// Ordinals are what the model cites by, so they have to be contiguous from one.
	for index, item := range served {
		if item.Ordinal != index+1 {
			t.Errorf("evidence %d carries ordinal %d, want %d", index, item.Ordinal, index+1)
		}
		if item.Statement == "" {
			t.Errorf("evidence %d states nothing", index+1)
		}
	}
}

// A read outside the case's scope must still be refused here, exactly as the runner would refuse
// it. The scenario is not a way around validation.
func TestServe_RefusesAReadOutsideTheCasesScope(t *testing.T) {
	brief := redHerringBrief()

	proposals := []investigation.Proposal{{
		CapabilityID:      capability.KubernetesContainerLogs,
		CapabilityVersion: capability.SchemaVersion1,
		Reason:            "read a pod from another workload",
		Arguments: investigation.Arguments{
			Namespace:     brief.Resource.Namespace,
			WorkloadKind:  investigation.WorkloadDeployment,
			WorkloadName:  brief.Resource.Name,
			PodName:       "some-other-workload-abcde",
			ContainerName: "checkout",
			Window:        brief.Window,
		},
	}}

	served, refused := serve(proposals, brief)
	if len(refused) != 1 {
		t.Fatalf("a pod the brief never resolved was not refused: served %d, refused %v",
			len(served), refused)
	}
}

// The scenario's own claim has to hold: the discriminating pod is on a different ReplicaSet from
// the two that took the new image. Without that, the red herring is not a red herring.
func TestScenario_HasAPodThatNeverTookTheNewImage(t *testing.T) {
	brief := redHerringBrief()
	if len(brief.Topology) != 3 {
		t.Fatalf("the scenario has %d pods, want 3", len(brief.Topology))
	}

	owners := make(map[string]int)
	for _, fact := range brief.Topology {
		owners[fact.Owner]++
	}
	if len(owners) != 2 {
		t.Fatalf("every pod has the same owner, so nothing distinguishes the deploy from the "+
			"cause: %v", owners)
	}
	for _, fact := range brief.Topology {
		if fact.Ready {
			t.Errorf("pod %s is Ready, but the incident is that none of them is", fact.Pod)
		}
	}
}

// Whichever pod is asked about, the log has to say the same thing — that is what takes the deploy
// out of the running rather than leaving it a coin flip.
func TestEvidence_TheLogBlamesTheCredentialWhicheverPodIsRead(t *testing.T) {
	brief := redHerringBrief()
	for _, fact := range brief.Topology {
		items := containerLog(fact.Pod, true)
		if len(items) != 1 {
			t.Fatalf("pod %s produced %d log items, want 1", fact.Pod, len(items))
		}
		if !strings.Contains(items[0].Content, "password authentication failed") {
			t.Errorf("the log for pod %s does not name the cause", fact.Pod)
		}
		if strings.Contains(items[0].Content, "2.8.0") {
			t.Errorf("the log for pod %s mentions the old image, which muddies the discriminator",
				fact.Pod)
		}
	}
}
