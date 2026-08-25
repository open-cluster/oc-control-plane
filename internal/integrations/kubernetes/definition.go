// Package kubernetes is the Kubernetes provider: a cluster read through a Relay by bounded
// typed reads — what is running, what the cluster said about it, and what a container
// logged before it died.
//
// The typed Relay Capability contracts live under internal/relay/capability because they
// are the closed wire operations shared with the Relay, not generic Integration features.
package kubernetes

import (
	"context"
	"fmt"
	"sort"
	"strings"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/relay/capability"
)

// Definition is what this provider exports to the catalog. Metadata mirrors the seeded
// integration_type row; a test proves the two agree.
type Executor interface {
	Execute(context.Context, integrations.ToolRequest, string, *relayv1.CapabilityArguments) (integrations.ToolResult, error)
}

func Definition(executors ...Executor) integrations.Definition {
	var executor Executor
	if len(executors) > 0 {
		executor = executors[0]
	}
	return integrations.Definition{
		ID:   integrations.TypeKubernetes,
		Key:  "kubernetes",
		Name: "Kubernetes",
		Description: "Give investigations read-only access to Kubernetes workload " +
			"runtime, namespace events, and bounded container logs through an outbound Relay.",
		Logo:     "kubernetes",
		Category: integrations.CategoryInfrastructure,
		DocumentationURL: "https://kubernetes.io/docs/reference/access-authn-authz/" +
			"rbac/#role-and-clusterrole",
		// There is deliberately no "cluster name" field. The Integration is already named
		// by the operator, and the cluster itself is pinned by the fingerprint the Relay
		// attested at enrolment — a third name for the same thing is a field that can
		// disagree with two others.
		Config: []integrations.Field{
			{
				Name: "namespaceAllowList",
				Title: "Namespaces this integration may read (comma separated; " +
					"empty means every namespace the Relay's service account can reach)",
				Description: "A narrowing on top of the Relay's own permissions, never a " +
					"widening: this cannot grant access the service account does not already have.",
				Type: integrations.FieldString,
			},
		},
		// Relay only. A cluster's API server is usually private, which is the whole reason
		// the Relay exists.
		RequiresRelay:    true,
		ReceivesWebhooks: false,
		Verify:           verify,
		Tools:            tools(executor),
	}
}

func relayCapabilities() []string {
	return []string{
		capability.KubernetesWorkloadRuntime,
		capability.KubernetesNamespaceEvents,
		capability.KubernetesContainerLogs,
	}
}

func tools(executor Executor) []integrations.Tool {
	run := func(id string, declared []integrations.ToolArgument) func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
		return func(ctx context.Context, request integrations.ToolRequest) (integrations.ToolResult, error) {
			if executor == nil {
				return integrations.ToolResult{}, fmt.Errorf("relay Tool execution is not configured")
			}
			arguments, err := argumentsFor(id, request, declared)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			return executor.Execute(ctx, request, id, arguments)
		}
	}
	workloadArguments := []integrations.ToolArgument{
		{Name: "namespace", Description: "namespace to read", Type: integrations.FieldString, Required: true},
		{Name: "workloadKind", Description: "Deployment, StatefulSet, or DaemonSet", Type: integrations.FieldString, Required: true},
		{Name: "workloadName", Description: "workload name", Type: integrations.FieldString, Required: true},
		{Name: "maxPods", Description: "maximum pods", Type: integrations.FieldInteger},
	}
	eventArguments := []integrations.ToolArgument{
		{Name: "namespace", Description: "namespace to read", Type: integrations.FieldString, Required: true},
		{Name: "maxEvents", Description: "maximum events", Type: integrations.FieldInteger},
	}
	logArguments := []integrations.ToolArgument{
		{Name: "namespace", Description: "namespace to read", Type: integrations.FieldString, Required: true},
		{Name: "podName", Description: "pod name", Type: integrations.FieldString, Required: true},
		{Name: "containerName", Description: "container name", Type: integrations.FieldString, Required: true},
		{Name: "maxLines", Description: "maximum lines", Type: integrations.FieldInteger},
		{Name: "maxBytes", Description: "maximum bytes", Type: integrations.FieldInteger},
	}
	return []integrations.Tool{
		{
			Name: capability.KubernetesWorkloadRuntime, Description: "Read the current runtime state of one workload.",
			WhenToUse:    "checking whether a named workload is available and which pods serve it",
			WhenNotToUse: "listing a namespace or reading logs", Permissions: "Relay service-account read access to the workload and pods",
			Arguments: workloadArguments,
			Requires:  []string{capability.KubernetesWorkloadRuntime},
			Output:    "bounded workload and pod runtime state", Run: run(capability.KubernetesWorkloadRuntime, workloadArguments),
		},
		{
			Name: capability.KubernetesNamespaceEvents, Description: "Read bounded Kubernetes events in one namespace.",
			WhenToUse:    "checking what Kubernetes reported during the investigation window",
			WhenNotToUse: "reading application logs", Permissions: "Relay service-account list access to events",
			Arguments: eventArguments,
			Requires:  []string{capability.KubernetesNamespaceEvents},
			Output:    "bounded events with source timestamps and truncation", Run: run(capability.KubernetesNamespaceEvents, eventArguments),
		},
		{
			Name: capability.KubernetesContainerLogs, Description: "Read a bounded tail from one named container.",
			WhenToUse:    "checking logs for a pod and container already identified",
			WhenNotToUse: "searching every pod or reading outside the investigation window", Permissions: "Relay service-account read access to pod logs",
			Arguments: logArguments,
			Requires:  []string{capability.KubernetesContainerLogs},
			Output:    "bounded log lines with truncation", Run: run(capability.KubernetesContainerLogs, logArguments),
		},
	}
}

// verify judges this integration from the bound Relay's own state: whether it is
// connected right now, and whether it advertised each Relay Capability this type declares. A
// Relay Capability the Relay did not advertise is the cluster's own configuration answering, not
// this platform's.
func verify(input integrations.VerifyInput) integrations.Verification {
	if !input.Relay.Bound {
		return integrations.Verification{
			Status: integrations.StatusFailed,
			Note:   "no relay serves this integration; bind one and verify again",
		}
	}
	if !input.Relay.Connected {
		return integrations.Verification{
			Status: integrations.StatusFailed,
			Note:   "the relay serving this integration is not connected",
		}
	}

	advertised := make(map[string]bool, len(input.Relay.Capabilities))
	for _, name := range input.Relay.Capabilities {
		advertised[name] = true
	}
	var missing []string
	for _, needed := range relayCapabilities() {
		if !advertised[needed] {
			missing = append(missing, needed)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		var granted []string
		for _, known := range relayCapabilities() {
			if advertised[known] {
				granted = append(granted, known)
			}
		}
		return integrations.Verification{
			Status: integrations.StatusDegraded,
			Note: "the relay is connected and does not advertise: " +
				strings.Join(missing, ", "),
			Grants: granted,
		}
	}
	return integrations.Verification{
		Status: integrations.StatusActive,
		Note:   "the relay is connected and advertises every Relay Capability this type declares",
		Grants: relayCapabilities(),
	}
}
