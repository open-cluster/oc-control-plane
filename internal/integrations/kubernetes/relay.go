package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/relay/capability"
	"github.com/open-cluster/oc-control-plane/internal/storage"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

type RelayExecutor struct{ Database *storage.Database }

func (e RelayExecutor) Execute(ctx context.Context, request integrations.ToolRequest, id string,
	arguments *relayv1.CapabilityArguments) (integrations.ToolResult, error) {
	organization, err := tenancy.NewOrganization(request.Integration.OrgID)
	if err != nil {
		return integrations.ToolResult{}, err
	}
	if request.Integration.RelayID == uuid.Nil {
		return integrations.ToolResult{}, errors.New("the Integration has no Relay binding")
	}
	encoded, err := proto.Marshal(arguments)
	if err != nil {
		return integrations.ToolResult{}, fmt.Errorf("encoding Relay Capability arguments: %w", err)
	}
	if err = capability.Validate(id, capability.SchemaVersion1, encoded); err != nil {
		return integrations.ToolResult{}, err
	}
	jobID := uuid.New()
	err = e.Database.EnqueueVerifiedJob(ctx, organization, storage.Job{
		ID: jobID, InvestigationID: request.InvestigationID,
		IntegrationID: request.Integration.ID, RegistrationID: request.Integration.RelayID,
		CapabilityID: id, CapabilityVersion: capability.SchemaVersion1, Arguments: encoded})
	if err != nil {
		return integrations.ToolResult{}, err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		outcome, terminal, readErr := e.Database.JobOutcome(ctx, organization, jobID)
		if readErr != nil {
			if ctx.Err() != nil {
				e.cancelJob(ctx, organization, jobID)
				return integrations.ToolResult{}, ctx.Err()
			}
			return integrations.ToolResult{}, readErr
		}
		if terminal {
			if outcome.Status != storage.JobSucceeded {
				return integrations.ToolResult{}, fmt.Errorf("relay capability ended with status %d", outcome.Status)
			}
			return decodeRelayResult(id, outcome.Result, request)
		}
		select {
		case <-ctx.Done():
			e.cancelJob(ctx, organization, jobID)
			return integrations.ToolResult{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (e RelayExecutor) cancelJob(ctx context.Context, organization tenancy.Organization, id uuid.UUID) {
	cancellation, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, _ = e.Database.RequestJobCancellation(cancellation, organization, id)
}

func decodeRelayResult(id string, encoded []byte, request integrations.ToolRequest) (integrations.ToolResult, error) {
	result := &relayv1.CapabilityResult{}
	if err := proto.Unmarshal(encoded, result); err != nil {
		return integrations.ToolResult{}, errors.New("relay result did not decode")
	}
	truncated := false
	var summary string
	var sources []string
	var windowFrom, windowUntil time.Time
	namespace, _ := request.Arguments["namespace"].(string)
	switch id {
	case capability.KubernetesWorkloadRuntime:
		workload := result.GetKubernetesWorkloadRuntimeV1()
		if workload == nil {
			return integrations.ToolResult{}, errors.New("relay returned a different result type")
		}
		if workload.GetOutcome() != relayv1.KubernetesReadOutcome_KUBERNETES_READ_OUTCOME_SUCCESS {
			return integrations.ToolResult{}, relayOutcomeError(workload.GetOutcome().String())
		}
		count := len(workload.GetPods())
		if workload.GetReturnedPodCount() != int32(count) || count > capability.MaxPods ||
			workload.GetAppliedMaxPods() > uint32(requestedBound(request, "maxPods", 20)) ||
			(workload.GetAppliedMaxPods() > 0 && count > int(workload.GetAppliedMaxPods())) {
			return integrations.ToolResult{}, errors.New("relay pod count exceeds its declared bounds")
		}
		truncated = !workload.GetComplete()
		kind, _ := request.Arguments["workloadKind"].(string)
		name, _ := request.Arguments["workloadName"].(string)
		sources = []string{namespace + "/" + strings.ToLower(kind) + "/" + name}
		summary = fmt.Sprintf("%d pod(s) for %s %s/%s", count, kind, namespace, name)
	case capability.KubernetesNamespaceEvents:
		windowFrom, windowUntil = eventWindow(request)
		effective := request
		effective.WindowFrom, effective.WindowUntil = windowFrom, windowUntil
		events := result.GetKubernetesNamespaceEventsV1()
		if events == nil {
			return integrations.ToolResult{}, errors.New("relay returned a different result type")
		}
		if events.GetOutcome() != relayv1.KubernetesEventsOutcome_KUBERNETES_EVENTS_OUTCOME_SUCCESS {
			return integrations.ToolResult{}, relayOutcomeError(events.GetOutcome().String())
		}
		count := len(events.GetEvents())
		if events.GetReturnedEventCount() != int32(count) || count > capability.MaxEvents ||
			events.GetAppliedMaxEvents() > uint32(requestedBound(request, "maxEvents", 100)) ||
			(events.GetAppliedMaxEvents() > 0 && count > int(events.GetAppliedMaxEvents())) {
			return integrations.ToolResult{}, errors.New("relay event count exceeds its declared bounds")
		}
		for _, event := range events.GetEvents() {
			if outsideWindow(event.GetLastSeenAt(), effective) {
				return integrations.ToolResult{}, errors.New("relay event is outside the investigation window")
			}
		}
		truncated = !events.GetComplete() || events.GetWindowPredatesRetention()
		sources = []string{namespace + "/events"}
		summary = fmt.Sprintf("%d event(s) in namespace %s", count, namespace)
	case capability.KubernetesContainerLogs:
		windowFrom, windowUntil = request.WindowFrom, request.WindowUntil
		logs := result.GetKubernetesContainerLogsV1()
		if logs == nil {
			return integrations.ToolResult{}, errors.New("relay returned a different result type")
		}
		if logs.GetOutcome() != relayv1.KubernetesLogsOutcome_KUBERNETES_LOGS_OUTCOME_SUCCESS {
			return integrations.ToolResult{}, relayOutcomeError(logs.GetOutcome().String())
		}
		count := len(logs.GetLines())
		if logs.GetReturnedLineCount() != int32(count) || count > capability.MaxLines ||
			logs.GetAppliedMaxLines() > uint32(requestedBound(request, "maxLines", 500)) ||
			(logs.GetAppliedMaxLines() > 0 && count > int(logs.GetAppliedMaxLines())) {
			return integrations.ToolResult{}, errors.New("relay log line count exceeds its declared bounds")
		}
		var actualBytes int64
		for _, line := range logs.GetLines() {
			actualBytes += int64(len(line.GetContent()))
			if outsideWindow(line.GetAt(), request) {
				return integrations.ToolResult{}, errors.New("relay log line is outside the investigation window")
			}
		}
		maxBytes := requestedBound(request, "maxBytes", 65536)
		if actualBytes > int64(maxBytes) || logs.GetReturnedByteCount() < actualBytes ||
			logs.GetReturnedByteCount() > int64(maxBytes) || logs.GetAppliedMaxBytes() > uint32(maxBytes) ||
			(logs.GetAppliedMaxBytes() > 0 && actualBytes > int64(logs.GetAppliedMaxBytes())) {
			return integrations.ToolResult{}, errors.New("relay log byte count exceeds its declared bounds")
		}
		truncated = !logs.GetComplete() || logs.GetTruncatedByLineBound() || logs.GetTruncatedByByteBound()
		pod, _ := request.Arguments["podName"].(string)
		container, _ := request.Arguments["containerName"].(string)
		sources = []string{namespace + "/" + pod + "/" + container}
		summary = fmt.Sprintf("%d log line(s) from %s/%s/%s", count, namespace, pod, container)
	default:
		return integrations.ToolResult{}, capability.ErrUnknownCapability
	}
	encodedJSON, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(result)
	if err != nil {
		return integrations.ToolResult{}, errors.New("relay result could not be rendered")
	}
	var content map[string]any
	if err = json.Unmarshal(encodedJSON, &content); err != nil {
		return integrations.ToolResult{}, err
	}
	return integrations.ToolResult{Content: content, Truncated: truncated, Sources: sources,
		Summary: summary, WindowFrom: windowFrom, WindowUntil: windowUntil}, nil
}

func eventWindow(request integrations.ToolRequest) (time.Time, time.Time) {
	until := request.WindowUntil
	if until.IsZero() {
		until = time.Now().UTC()
	}
	from := request.WindowFrom
	if from.IsZero() {
		from = until.Add(-time.Hour)
	}
	if from.Before(until.Add(-capability.MaxEventWindow)) {
		from = until.Add(-capability.MaxEventWindow)
	}
	return from, until
}

func relayOutcomeError(outcome string) error {
	return fmt.Errorf("relay read failed: %s", strings.ToLower(outcome))
}

func requestedBound(request integrations.ToolRequest, name string, fallback int) int {
	if value, ok := request.Arguments[name].(float64); ok {
		return int(value)
	}
	return fallback
}

func outsideWindow(at *timestamppb.Timestamp, request integrations.ToolRequest) bool {
	if at == nil {
		return false
	}
	if at.CheckValid() != nil {
		return true
	}
	observed := at.AsTime()
	return !request.WindowFrom.IsZero() && observed.Before(request.WindowFrom) ||
		!request.WindowUntil.IsZero() && observed.After(request.WindowUntil)
}

func argumentsFor(id string, request integrations.ToolRequest, declared []integrations.ToolArgument) (*relayv1.CapabilityArguments, error) {
	values, err := integrations.ReadArguments(declared, request.Arguments)
	if err != nil {
		return nil, err
	}
	namespace, err := values.Required("namespace")
	if err != nil {
		return nil, err
	}
	if !namespaceAllowed(request.Integration, namespace) {
		return nil, errors.New("namespace is outside this Integration's allow list")
	}
	switch id {
	case capability.KubernetesWorkloadRuntime:
		workloadKind, kindErr := values.Required("workloadKind")
		if kindErr != nil {
			return nil, kindErr
		}
		workloadName, nameErr := values.Required("workloadName")
		if nameErr != nil {
			return nil, nameErr
		}
		maxPods, countErr := values.Count("maxPods", 20, capability.MaxPods)
		if countErr != nil {
			return nil, countErr
		}
		var kind relayv1.WorkloadKind
		switch strings.ToLower(workloadKind) {
		case "deployment":
			kind = relayv1.WorkloadKind_WORKLOAD_KIND_DEPLOYMENT
		case "statefulset":
			kind = relayv1.WorkloadKind_WORKLOAD_KIND_STATEFULSET
		case "daemonset":
			kind = relayv1.WorkloadKind_WORKLOAD_KIND_DAEMONSET
		default:
			return nil, errors.New("workloadKind must be Deployment, StatefulSet, or DaemonSet")
		}
		return &relayv1.CapabilityArguments{Arguments: &relayv1.CapabilityArguments_KubernetesWorkloadRuntimeV1{KubernetesWorkloadRuntimeV1: &relayv1.KubernetesWorkloadRuntimeArgsV1{
			Namespace: namespace, WorkloadKind: kind, WorkloadName: workloadName, MaxPods: uint32(maxPods)}}}, nil
	case capability.KubernetesNamespaceEvents:
		maxEvents, countErr := values.Count("maxEvents", 100, capability.MaxEvents)
		if countErr != nil {
			return nil, countErr
		}
		from, until := eventWindow(request)
		if from.After(until) {
			return nil, errors.New("investigation window starts after it ends")
		}
		return &relayv1.CapabilityArguments{Arguments: &relayv1.CapabilityArguments_KubernetesNamespaceEventsV1{KubernetesNamespaceEventsV1: &relayv1.KubernetesNamespaceEventsArgsV1{
			Namespace: namespace, WindowStart: timestamppb.New(from), WindowEnd: timestamppb.New(until), MaxEvents: uint32(maxEvents)}}}, nil
	case capability.KubernetesContainerLogs:
		podName, podErr := values.Required("podName")
		if podErr != nil {
			return nil, podErr
		}
		containerName, containerErr := values.Required("containerName")
		if containerErr != nil {
			return nil, containerErr
		}
		maxLines, linesErr := values.Count("maxLines", 500, capability.MaxLines)
		if linesErr != nil {
			return nil, linesErr
		}
		maxBytes, bytesErr := values.Count("maxBytes", 65536, capability.MaxBytes)
		if bytesErr != nil {
			return nil, bytesErr
		}
		var since *timestamppb.Timestamp
		if !request.WindowFrom.IsZero() {
			since = timestamppb.New(request.WindowFrom)
		}
		return &relayv1.CapabilityArguments{Arguments: &relayv1.CapabilityArguments_KubernetesContainerLogsV1{KubernetesContainerLogsV1: &relayv1.KubernetesContainerLogsArgsV1{
			Namespace: namespace, PodName: podName, ContainerName: containerName, MaxLines: uint32(maxLines), MaxBytes: uint32(maxBytes), SinceTime: since}}}, nil
	default:
		return nil, capability.ErrUnknownCapability
	}
}

func namespaceAllowed(integration integrations.Integration, namespace string) bool {
	raw, _ := integration.Configuration["namespaceAllowList"].(string)
	if strings.TrimSpace(raw) == "" {
		return true
	}
	for _, allowed := range strings.Split(raw, ",") {
		if strings.TrimSpace(allowed) == namespace {
			return true
		}
	}
	return false
}
