package kubernetes

import (
	"context"
	"strings"
	"testing"
	"time"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/relay/capability"
)

// Verification judges the bound Relay's own state, and every answer names what is wrong in
// the operator's language: a missing binding, a dead session, or a Relay Capability the Relay
// did not advertise.
func TestVerify_JudgesTheRelayHonestly(t *testing.T) {
	t.Parallel()

	all := relayCapabilities()

	for _, testCase := range []struct {
		name       string
		relay      integrations.RelayStatus
		wantStatus integrations.Status
		wantInNote string
	}{
		{"no relay bound",
			integrations.RelayStatus{},
			integrations.StatusFailed, "no relay serves"},
		{"relay not connected",
			integrations.RelayStatus{Bound: true},
			integrations.StatusFailed, "not connected"},
		{"relay missing a Relay Capability",
			integrations.RelayStatus{Bound: true, Connected: true, Capabilities: all[:1]},
			integrations.StatusDegraded, "does not advertise"},
		{"relay advertising everything",
			integrations.RelayStatus{Bound: true, Connected: true, Capabilities: all},
			integrations.StatusActive, "every Relay Capability"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			verified := Definition().Verify(integrations.VerifyInput{Relay: testCase.relay})
			if verified.Status != testCase.wantStatus {
				t.Errorf("verified as %v, want %v; note: %s",
					verified.Status, testCase.wantStatus, verified.Note)
			}
			if !strings.Contains(verified.Note, testCase.wantInNote) {
				t.Errorf("the note %q does not say %q", verified.Note, testCase.wantInNote)
			}
		})
	}
}

type executorCapture struct {
	ids       []string
	arguments []*relayv1.CapabilityArguments
}

func (e *executorCapture) Execute(
	_ context.Context, _ integrations.ToolRequest, id string, arguments *relayv1.CapabilityArguments,
) (integrations.ToolResult, error) {
	e.ids = append(e.ids, id)
	e.arguments = append(e.arguments, arguments)
	return integrations.ToolResult{Summary: id}, nil
}

func TestEveryKubernetesToolDispatchesItsTypedRelayCapability(t *testing.T) {
	t.Parallel()

	executor := &executorCapture{}
	definition := Definition(executor)
	request := integrations.ToolRequest{
		Integration: integrations.Integration{
			Configuration: map[string]any{"namespaceAllowList": "shop,platform"},
		},
		WindowFrom: time.Now().Add(-time.Hour), WindowUntil: time.Now(),
	}
	arguments := []map[string]any{
		{"namespace": "shop", "workloadKind": "Deployment", "workloadName": "checkout"},
		{"namespace": "shop"},
		{"namespace": "shop", "podName": "checkout-1", "containerName": "api"},
	}
	for index, tool := range definition.Tools {
		request.Arguments = arguments[index]
		if _, err := tool.Run(context.Background(), request); err != nil {
			t.Fatalf("running %s: %v", tool.Name, err)
		}
	}
	want := []string{
		capability.KubernetesWorkloadRuntime,
		capability.KubernetesNamespaceEvents,
		capability.KubernetesContainerLogs,
	}
	if strings.Join(executor.ids, ",") != strings.Join(want, ",") {
		t.Fatalf("dispatched %v, want %v", executor.ids, want)
	}

	request.Arguments = arguments[0]
	request.Arguments["namespace"] = "other"
	if _, err := definition.Tools[0].Run(context.Background(), request); err == nil {
		t.Fatal("a namespace outside the Integration allow list was dispatched")
	}
}

func TestKubernetesToolsRefuseArgumentsOutsideTheirDeclaredContract(t *testing.T) {
	t.Parallel()

	executor := &executorCapture{}
	tool := Definition(executor).Tools[0]
	request := integrations.ToolRequest{Arguments: map[string]any{
		"namespace": "shop", "workloadKind": "Deployment", "workloadName": "checkout",
		"undeclared": "must not be silently discarded",
	}}
	if _, err := tool.Run(context.Background(), request); err == nil {
		t.Fatal("an undeclared argument reached the Relay executor")
	}
	if len(executor.ids) != 0 {
		t.Fatalf("the invalid call was dispatched: %v", executor.ids)
	}
}

func TestKubernetesToolsEnforceTypedBoundsAndInvestigationWindows(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name      string
		toolIndex int
		arguments map[string]any
	}{
		{name: "missing namespace", toolIndex: 1, arguments: map[string]any{}},
		{name: "unknown workload kind", arguments: map[string]any{
			"namespace": "shop", "workloadKind": "CronJob", "workloadName": "checkout",
		}},
		{name: "fractional pod bound", arguments: map[string]any{
			"namespace": "shop", "workloadKind": "Deployment", "workloadName": "checkout", "maxPods": 1.5,
		}},
		{name: "oversized event bound", toolIndex: 1, arguments: map[string]any{
			"namespace": "shop", "maxEvents": float64(capability.MaxEvents + 1),
		}},
		{name: "missing container", toolIndex: 2, arguments: map[string]any{
			"namespace": "shop", "podName": "checkout-1",
		}},
		{name: "oversized log bytes", toolIndex: 2, arguments: map[string]any{
			"namespace": "shop", "podName": "checkout-1", "containerName": "api",
			"maxBytes": float64(capability.MaxBytes + 1),
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			executor := &executorCapture{}
			tool := Definition(executor).Tools[testCase.toolIndex]
			request := integrations.ToolRequest{
				Arguments: testCase.arguments, WindowFrom: time.Now().Add(-time.Hour), WindowUntil: time.Now(),
			}
			if _, err := tool.Run(context.Background(), request); err == nil {
				t.Fatal("an invalid read reached the Relay executor")
			}
			if len(executor.ids) != 0 {
				t.Fatalf("the invalid call was dispatched: %v", executor.ids)
			}
		})
	}

	executor := &executorCapture{}
	definition := Definition(executor)
	until := time.Now().UTC().Truncate(time.Second)
	from := until.Add(-8 * 24 * time.Hour)
	events := integrations.ToolRequest{
		Arguments: map[string]any{"namespace": "shop"}, WindowFrom: from, WindowUntil: until,
	}
	if _, err := definition.Tools[1].Run(context.Background(), events); err != nil {
		t.Fatalf("running namespace events: %v", err)
	}
	window := executor.arguments[0].GetKubernetesNamespaceEventsV1()
	if got := window.GetWindowStart().AsTime(); !got.Equal(until.Add(-capability.MaxEventWindow)) {
		t.Errorf("event window starts at %s, want the seven-day protocol bound", got)
	}

	logs := integrations.ToolRequest{
		Arguments: map[string]any{
			"namespace": "shop", "podName": "checkout-1", "containerName": "api",
		},
		WindowFrom: from, WindowUntil: until,
	}
	if _, err := definition.Tools[2].Run(context.Background(), logs); err != nil {
		t.Fatalf("running container logs: %v", err)
	}
	logArguments := executor.arguments[1].GetKubernetesContainerLogsV1()
	if logArguments.GetSinceTime() == nil || !logArguments.GetSinceTime().AsTime().Equal(from) {
		t.Errorf("log since time = %v, want investigation start %s", logArguments.GetSinceTime(), from)
	}
}

func TestDefinition_DeclaresTheRelayShape(t *testing.T) {
	t.Parallel()

	definition := Definition()
	if definition.ID != integrations.TypeKubernetes || definition.Key != "kubernetes" {
		t.Errorf("the definition's identity is (%d, %q)", definition.ID, definition.Key)
	}
	if definition.Category != integrations.Category("infrastructure") {
		t.Errorf("category = %q, want infrastructure", definition.Category)
	}
	if !definition.RequiresRelay || definition.ReceivesWebhooks {
		t.Error("kubernetes is relay-served and receives no webhooks")
	}
	if len(definition.Tools) != 3 {
		t.Errorf("kubernetes declares %d Tools, want the three typed reads",
			len(definition.Tools))
	}
	const wantDescription = "Give investigations read-only access to Kubernetes workload " +
		"runtime, namespace events, and bounded container logs through an outbound Relay."
	if definition.Description != wantDescription {
		t.Errorf("description = %q, want %q", definition.Description, wantDescription)
	}
}
