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

type Executor interface {
	Execute(context.Context, integrations.ToolRequest, string, *relayv1.CapabilityArguments) (integrations.ToolResult, error)
}

func Definition(executors ...Executor) integrations.Definition {
	var executor Executor
	if len(executors) > 0 {
		executor = executors[0]
	}
	return integrations.Definition{
		Manifest: integrations.Manifest{
			Key:  "kubernetes",
			Name: "Kubernetes",
			Description: "Give investigations read-only access to Kubernetes workload " +
				"runtime, namespace events, and bounded container logs through an outbound Relay.",
			Logo: "kubernetes", Category: integrations.CategoryInfrastructure,
			SourceURL:         "https://kubernetes.io/docs/reference/access-authn-authz/rbac/#role-and-clusterrole",
			DocumentationSlug: "integrations/infrastructure/kubernetes",
			Config: []integrations.Field{
				{
					Key: "namespaceAllowList",
					Label: "Namespaces this integration may read (comma separated; " +
						"empty means every namespace the Relay's service account can reach)",
					Type: integrations.FieldString,
				},
			},
			RequiresRelay: true, Tools: tools(executor),
		},
		Verify: verify,
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
			Name: capability.KubernetesWorkloadRuntime,
			Description: "Read the current runtime state of one named workload and the pods " +
				"serving it. Use to check present availability and pod state. Do not use for " +
				"namespace discovery, logs, or historical state; current state does not prove " +
				"what was running earlier. Results are bounded and report truncation.",
			Arguments:      workloadArguments,
			RequiredGrants: []string{capability.KubernetesWorkloadRuntime},
			Run:            run(capability.KubernetesWorkloadRuntime, workloadArguments),
		},
		{
			Name: capability.KubernetesNamespaceEvents,
			Description: "Read bounded Kubernetes events from one namespace in the " +
				"Investigation window. Use to check what Kubernetes reported about scheduling, " +
				"health, and lifecycle changes. Do not use for application logs. Results retain " +
				"source timestamps and report truncation.",
			Arguments:      eventArguments,
			RequiredGrants: []string{capability.KubernetesNamespaceEvents},
			Run:            run(capability.KubernetesNamespaceEvents, eventArguments),
		},
		{
			Name: capability.KubernetesContainerLogs,
			Description: "Read a bounded log tail from one named container in a known pod. " +
				"Use after workload state identifies the pod and container relevant to the " +
				"Investigation. Do not scan every pod or use this outside the Investigation " +
				"window. Results report truncation.",
			Arguments:      logArguments,
			RequiredGrants: []string{capability.KubernetesContainerLogs},
			Run:            run(capability.KubernetesContainerLogs, logArguments),
		},
	}
}

func verify(input integrations.VerifyInput) integrations.Verification {
	if !input.RelayStatus.Bound {
		return integrations.Verification{
			Status: integrations.StatusFailed,
			Note:   "no relay serves this integration; bind one and verify again",
		}
	}
	if !input.RelayStatus.Connected {
		return integrations.Verification{
			Status: integrations.StatusFailed,
			Note:   "the relay serving this integration is not connected",
		}
	}

	advertised := make(map[string]bool, len(input.RelayStatus.Capabilities))
	for _, name := range input.RelayStatus.Capabilities {
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
			Status: integrations.StatusVerified,
			Note: "the relay is connected and does not advertise: " +
				strings.Join(missing, ", "),
			Grants: granted,
		}
	}
	return integrations.Verification{
		Status: integrations.StatusVerified,
		Note:   "the relay is connected and advertises every Relay Capability this type declares",
		Grants: relayCapabilities(),
	}
}
