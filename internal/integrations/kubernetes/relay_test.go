package kubernetes

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/relay/capability"
)

func TestRelayResultsPreserveTypedFailuresBoundsWindowsAndSources(t *testing.T) {
	t.Parallel()

	until := time.Now().UTC().Truncate(time.Second)
	from := until.Add(-time.Hour)
	request := integrations.ToolRequest{
		Arguments: map[string]any{
			"namespace": "shop", "workloadKind": "Deployment", "workloadName": "checkout",
			"podName": "checkout-1", "containerName": "api",
		},
		WindowFrom: from, WindowUntil: until,
	}
	tests := []struct {
		name       string
		id         string
		result     *relayv1.CapabilityResult
		wantError  string
		wantSource string
	}{
		{
			name: "unauthorized workload is not an empty success", id: capability.KubernetesWorkloadRuntime,
			result: &relayv1.CapabilityResult{Result: &relayv1.CapabilityResult_KubernetesWorkloadRuntimeV1{
				KubernetesWorkloadRuntimeV1: &relayv1.KubernetesWorkloadRuntimeResultV1{
					Outcome: relayv1.KubernetesReadOutcome_KUBERNETES_READ_OUTCOME_UNAUTHORIZED,
				},
			}}, wantError: "unauthorized",
		},
		{
			name: "unreachable event read is not an empty success", id: capability.KubernetesNamespaceEvents,
			result: &relayv1.CapabilityResult{Result: &relayv1.CapabilityResult_KubernetesNamespaceEventsV1{
				KubernetesNamespaceEventsV1: &relayv1.KubernetesNamespaceEventsResultV1{
					Outcome: relayv1.KubernetesEventsOutcome_KUBERNETES_EVENTS_OUTCOME_UNREACHABLE,
				},
			}}, wantError: "unreachable",
		},
		{
			name: "missing container is not an empty success", id: capability.KubernetesContainerLogs,
			result: &relayv1.CapabilityResult{Result: &relayv1.CapabilityResult_KubernetesContainerLogsV1{
				KubernetesContainerLogsV1: &relayv1.KubernetesContainerLogsResultV1{
					Outcome: relayv1.KubernetesLogsOutcome_KUBERNETES_LOGS_OUTCOME_CONTAINER_NOT_FOUND,
				},
			}}, wantError: "container_not_found",
		},
		{
			name: "dishonest event count is refused", id: capability.KubernetesNamespaceEvents,
			result: &relayv1.CapabilityResult{Result: &relayv1.CapabilityResult_KubernetesNamespaceEventsV1{
				KubernetesNamespaceEventsV1: &relayv1.KubernetesNamespaceEventsResultV1{
					Outcome:            relayv1.KubernetesEventsOutcome_KUBERNETES_EVENTS_OUTCOME_SUCCESS,
					ReturnedEventCount: 2, AppliedMaxEvents: 100,
				},
			}}, wantError: "event count",
		},
		{
			name: "logs outside the investigation window are refused", id: capability.KubernetesContainerLogs,
			result: &relayv1.CapabilityResult{Result: &relayv1.CapabilityResult_KubernetesContainerLogsV1{
				KubernetesContainerLogsV1: &relayv1.KubernetesContainerLogsResultV1{
					Outcome: relayv1.KubernetesLogsOutcome_KUBERNETES_LOGS_OUTCOME_SUCCESS,
					Lines: []*relayv1.KubernetesLogLine{{
						At: timestamppb.New(from.Add(-time.Second)), Content: "outside",
					}}, ReturnedLineCount: 1, ReturnedByteCount: 7,
					AppliedMaxLines: 500, AppliedMaxBytes: 65536,
				},
			}}, wantError: "investigation window",
		},
		{
			name: "successful logs identify their actual source", id: capability.KubernetesContainerLogs,
			result: &relayv1.CapabilityResult{Result: &relayv1.CapabilityResult_KubernetesContainerLogsV1{
				KubernetesContainerLogsV1: &relayv1.KubernetesContainerLogsResultV1{
					Outcome: relayv1.KubernetesLogsOutcome_KUBERNETES_LOGS_OUTCOME_SUCCESS,
					Lines: []*relayv1.KubernetesLogLine{{
						At: timestamppb.New(from.Add(time.Second)), Content: "ready",
					}}, ReturnedLineCount: 1, ReturnedByteCount: 5, Complete: true,
					AppliedMaxLines: 500, AppliedMaxBytes: 65536,
				},
			}}, wantSource: "shop/checkout-1/api",
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			encoded, err := proto.Marshal(testCase.result)
			if err != nil {
				t.Fatal(err)
			}
			result, err := decodeRelayResult(testCase.id, encoded, request)
			if testCase.wantError != "" {
				if err == nil || !strings.Contains(strings.ToLower(err.Error()), testCase.wantError) {
					t.Fatalf("error = %v, want %q", err, testCase.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Sources) != 1 || result.Sources[0] != testCase.wantSource {
				t.Fatalf("sources = %v, want %q", result.Sources, testCase.wantSource)
			}
			if !strings.Contains(result.Summary, "1 log line") {
				t.Errorf("summary = %q, want a resource-specific result", result.Summary)
			}
		})
	}
}

func TestRelayResultsReportAndEnforceOnlyTheActuallyDispatchedWindow(t *testing.T) {
	t.Parallel()

	until := time.Now().UTC().Truncate(time.Second)
	request := integrations.ToolRequest{
		Arguments:  map[string]any{"namespace": "shop"},
		WindowFrom: until.Add(-30 * 24 * time.Hour), WindowUntil: until,
	}
	encoded, err := proto.Marshal(&relayv1.CapabilityResult{
		Result: &relayv1.CapabilityResult_KubernetesNamespaceEventsV1{
			KubernetesNamespaceEventsV1: &relayv1.KubernetesNamespaceEventsResultV1{
				Outcome: relayv1.KubernetesEventsOutcome_KUBERNETES_EVENTS_OUTCOME_SUCCESS,
				Events: []*relayv1.KubernetesEvent{{
					LastSeenAt: timestamppb.New(until.Add(-24 * time.Hour)),
				}}, ReturnedEventCount: 1, AppliedMaxEvents: 100, Complete: true,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := decodeRelayResult(capability.KubernetesNamespaceEvents, encoded, request)
	if err != nil {
		t.Fatal(err)
	}
	if want := until.Add(-capability.MaxEventWindow); !result.WindowFrom.Equal(want) ||
		!result.WindowUntil.Equal(until) {
		t.Fatalf("recorded event window = %s through %s, want %s through %s",
			result.WindowFrom, result.WindowUntil, want, until)
	}

	outside, err := proto.Marshal(&relayv1.CapabilityResult{
		Result: &relayv1.CapabilityResult_KubernetesNamespaceEventsV1{
			KubernetesNamespaceEventsV1: &relayv1.KubernetesNamespaceEventsResultV1{
				Outcome: relayv1.KubernetesEventsOutcome_KUBERNETES_EVENTS_OUTCOME_SUCCESS,
				Events: []*relayv1.KubernetesEvent{{
					LastSeenAt: timestamppb.New(until.Add(-9 * 24 * time.Hour)),
				}}, ReturnedEventCount: 1, AppliedMaxEvents: 100, Complete: true,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = decodeRelayResult(capability.KubernetesNamespaceEvents, outside, request); err == nil {
		t.Fatal("an event outside the dispatched seven-day window was accepted")
	}

	request.Arguments = map[string]any{
		"namespace": "shop", "workloadKind": "Deployment", "workloadName": "checkout",
	}
	snapshot, err := proto.Marshal(&relayv1.CapabilityResult{
		Result: &relayv1.CapabilityResult_KubernetesWorkloadRuntimeV1{
			KubernetesWorkloadRuntimeV1: &relayv1.KubernetesWorkloadRuntimeResultV1{
				Outcome:        relayv1.KubernetesReadOutcome_KUBERNETES_READ_OUTCOME_SUCCESS,
				AppliedMaxPods: 20, Complete: true,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err = decodeRelayResult(capability.KubernetesWorkloadRuntime, snapshot, request)
	if err != nil {
		t.Fatal(err)
	}
	if !result.WindowFrom.IsZero() || !result.WindowUntil.IsZero() {
		t.Fatalf("a current workload snapshot falsely claimed a historical window: %+v", result)
	}
}
