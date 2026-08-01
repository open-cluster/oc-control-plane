package investigation

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"

	"github.com/open-cluster/oc-control-plane/internal/capability"
)

// Turning what a capability read came back with into EvidenceItems or CoverageGaps.
//
// A result NEVER becomes an item directly. It is validated for grounding, provenance, scope and
// completeness first, and the two things that fall out of that check are an item or a gap — there
// is no third outcome and nothing is discarded. That is why every function below returns both
// halves together: a read that produced no item almost always produced a gap, and returning them
// apart is how the gap gets dropped.
//
// Everything here arrives from a Relay, so everything here is relay-attested. It is recorded as
// such and is never promoted to centrally verified.

// maxContentLines bounds how many log lines one evidence item's content carries before the item
// says so. It is below the capability's own line ceiling deliberately: the read is bounded by what
// the cluster will serve, and this is bounded by what a person can read at 03:00.
const maxContentLines = 200

// Interpret turns one settled read into evidence and gaps.
//
// A read that did not succeed still produces a gap, because "we asked and got nothing back" is
// something an engineer needs to know and is not the same as not having asked.
func Interpret(request Request, read Read, scope Scope, window Window, connection uuid.UUID) Validated {
	if !read.Succeeded {
		return Validated{Gaps: []Gap{failureGap(request, read)}}
	}

	result := &relayv1.CapabilityResult{}
	if err := proto.Unmarshal(read.Result, result); err != nil {
		return Validated{Gaps: []Gap{{
			Cause:        GapSourceUnreachable,
			CapabilityID: request.CapabilityID,
			Subject:      request.CapabilityID,
			Consequence: "the result could not be read, so nothing it may have contained is " +
				"available to this investigation",
		}}}
	}

	// What the customer's own policy withheld is read from the envelope rather than from any one
	// capability's payload, because the Relay masks at one point for every capability. Reading it
	// per capability would be three places for a fourth capability to be forgotten in.
	masking := MaskingFrom(result.GetRedaction())

	var validated Validated
	switch request.CapabilityID {
	case capability.KubernetesWorkloadRuntime:
		validated = interpretWorkload(
			request, result.GetKubernetesWorkloadRuntimeV1(), scope, connection, masking)
	case capability.KubernetesNamespaceEvents:
		validated = interpretEvents(
			request, result.GetKubernetesNamespaceEventsV1(), scope, window, connection, masking)
	case capability.KubernetesContainerLogs:
		validated = interpretLogs(
			request, result.GetKubernetesContainerLogsV1(), scope, connection, masking)
	default:
		return Validated{Gaps: []Gap{{
			Cause:        GapCapabilityUnavailable,
			CapabilityID: request.CapabilityID,
			Subject:      request.CapabilityID,
			Consequence:  "this build cannot interpret what that read returned",
		}}}
	}

	validated.Gaps = append(validated.Gaps, masking.Gaps(request)...)
	return validated
}

// failureGap describes a read that did not return. The kinds are told apart because they call for
// different things: a capability local policy refuses reads "not permitted by local policy" and
// never "not available", which is the distinction ADR-015 makes and the only one of the two that
// tells an operator what to do.
func failureGap(request Request, read Read) Gap {
	failure := &relayv1.JobFailure{}
	_ = proto.Unmarshal(read.Result, failure)

	cause, consequence := GapSourceUnreachable, "the read did not return, so what it would have "+
		"shown is not part of this investigation"
	switch failure.GetKind() {
	case relayv1.JobFailure_KIND_TIMEOUT:
		cause = GapResultTruncated
		consequence = "the read did not finish inside its timeout, so nothing it found can " +
			"support a claim that something was absent"
	case relayv1.JobFailure_KIND_CANCELLED:
		cause = GapResultTruncated
		consequence = "the read was stopped before it finished"
	case relayv1.JobFailure_KIND_UNSUPPORTED_CAPABILITY_VERSION:
		cause = GapCapabilityUnavailable
		consequence = "the installation serving this connection does not carry that read at the " +
			"version this build dispatches"
	case relayv1.JobFailure_KIND_ARGUMENTS_REJECTED:
		cause = GapRequestRefused
		consequence = "the installation refused the arguments, so the read was never made"
	case relayv1.JobFailure_KIND_RESULT_TOO_LARGE:
		cause = GapResultTruncated
		consequence = "the answer was larger than the bound this read carries, so it was not " +
			"returned and nothing about completeness can be said"
	case relayv1.JobFailure_KIND_LOCAL_POLICY_REFUSED:
		cause = GapCapabilityNotPermitted
		consequence = "this organization's own local policy refuses that read, so it was not made"
	}

	return Gap{
		Cause:        cause,
		CapabilityID: request.CapabilityID,
		Subject:      request.CapabilityID,
		Consequence:  consequence,
	}
}

// interpretWorkload reads what the cluster says the workload is and how its pods are running.
func interpretWorkload(
	request Request, result *relayv1.KubernetesWorkloadRuntimeResultV1,
	scope Scope, connection uuid.UUID, masking Masking,
) Validated {
	if result == nil {
		return Validated{Gaps: []Gap{unreadable(request)}}
	}

	subject := scope.Namespace + "/" + scope.WorkloadName
	switch result.GetOutcome() {
	case relayv1.KubernetesReadOutcome_KUBERNETES_READ_OUTCOME_UNREACHABLE:
		return Validated{Gaps: []Gap{{
			Cause: GapSourceUnreachable, CapabilityID: request.CapabilityID, Subject: subject,
			Consequence: "the cluster did not answer, so what the workload is doing is unknown",
		}}}
	case relayv1.KubernetesReadOutcome_KUBERNETES_READ_OUTCOME_UNAUTHORIZED:
		return Validated{Gaps: []Gap{{
			Cause: GapAuthorizationDenied, CapabilityID: request.CapabilityID, Subject: subject,
			Consequence: "the read was refused by the cluster's own authorization, so the " +
				"workload's state cannot be established and its absence cannot be concluded either",
		}}}
	case relayv1.KubernetesReadOutcome_KUBERNETES_READ_OUTCOME_WORKLOAD_NOT_FOUND,
		relayv1.KubernetesReadOutcome_KUBERNETES_READ_OUTCOME_NAMESPACE_NOT_FOUND:
		return Validated{Gaps: []Gap{{
			Cause: GapTargetNotFound, CapabilityID: request.CapabilityID, Subject: subject,
			Consequence: "the cluster does not have what this investigation was scoped to; a " +
				"not-found is not evidence that it never existed",
		}}}
	case relayv1.KubernetesReadOutcome_KUBERNETES_READ_OUTCOME_SELECTOR_UNREPRESENTABLE:
		return Validated{Gaps: []Gap{{
			Cause: GapResultTruncated, CapabilityID: request.CapabilityID,
			Subject: subject + " pod enumeration",
			Consequence: "the workload's selector could not be rendered, so its pods were not " +
				"enumerated rather than a namespace being listed instead",
		}}}
	case relayv1.KubernetesReadOutcome_KUBERNETES_READ_OUTCOME_SUCCESS:
	default:
		return Validated{Gaps: []Gap{unreadable(request)}}
	}

	certificate := &Certificate{
		SearchedScope:      subject,
		PaginationComplete: result.GetComplete(),
		FullyAuthorized:    true,
		MaskedFields:       masking.Fields(),
		AttestedBy:         TrustRelayAttested,
	}
	readAt := timeOf(result.GetReadAt())
	summary := result.GetWorkload()

	validated := Validated{}
	validated.Items = append(validated.Items, Item{
		RequestID:         request.ID,
		CapabilityID:      request.CapabilityID,
		CapabilityVersion: request.CapabilityVersion,
		Connection:        connection,
		Statement: fmt.Sprintf(
			"%s %s declares %d replicas: %d ready, %d available, %d updated (generation %d, "+
				"observed %d)",
			summary.GetKind(), subject, summary.GetDesiredReplicas(), summary.GetReadyReplicas(),
			summary.GetAvailableReplicas(), summary.GetUpdatedReplicas(),
			summary.GetGeneration(), summary.GetObservedGeneration()),
		Content:          strings.Join(summary.GetContainerImages(), "\n"),
		Trust:            TrustRelayAttested,
		Certificate:      certificate,
		SourceObservedAt: readAt,
		ReceivedAt:       time.Now(),
	})

	validated.Resource = &ResourceIdentity{
		Kind:               summary.GetKind(),
		Name:               summary.GetName(),
		Namespace:          summary.GetNamespace(),
		UID:                summary.GetUid(),
		DesiredReplicas:    summary.GetDesiredReplicas(),
		ReadyReplicas:      summary.GetReadyReplicas(),
		UpdatedReplicas:    summary.GetUpdatedReplicas(),
		AvailableReplicas:  summary.GetAvailableReplicas(),
		Generation:         summary.GetGeneration(),
		ObservedGeneration: summary.GetObservedGeneration(),
		ContainerImages:    summary.GetContainerImages(),
		CreatedAt:          timeOf(summary.GetCreatedAt()),
		Resolved:           true,
	}

	for _, pod := range result.GetPods() {
		validated.Topology = append(validated.Topology, BriefTopology{
			Pod:   pod.GetName(),
			Node:  pod.GetNodeName(),
			Owner: summary.GetKind() + "/" + summary.GetName(),
			Phase: pod.GetPhase(),
			Ready: pod.GetReady(),
			Item:  len(validated.Items),
		})
		validated.Items = append(validated.Items, Item{
			RequestID:         request.ID,
			CapabilityID:      request.CapabilityID,
			CapabilityVersion: request.CapabilityVersion,
			Connection:        connection,
			Statement:         podStatement(pod),
			Trust:             TrustRelayAttested,
			Certificate:       certificate,
			SourceObservedAt:  readAt,
			ReceivedAt:        time.Now(),
		})
	}

	if !result.GetComplete() {
		validated.Gaps = append(validated.Gaps, Gap{
			Cause: GapResultTruncated, CapabilityID: request.CapabilityID,
			Subject: subject + " pods",
			Consequence: fmt.Sprintf(
				"only %d pods were returned under a bound of %d, so nothing here supports a claim "+
					"that no other pod is affected",
				result.GetReturnedPodCount(), result.GetAppliedMaxPods()),
		})
	}
	return validated
}

// podStatement says what one pod is doing, in the terms an engineer would use. Container state is
// included because "the pod is not ready" is a symptom and "it exited with code 1 nine times" is a
// claim about it.
func podStatement(pod *relayv1.KubernetesPodRuntime) string {
	statement := &strings.Builder{}
	fmt.Fprintf(statement, "pod %s is %s", pod.GetName(), pod.GetPhase())
	if node := pod.GetNodeName(); node != "" {
		fmt.Fprintf(statement, " on node %s", node)
	}
	if !pod.GetReady() {
		statement.WriteString(", not ready")
	}
	if reason := pod.GetUnscheduledReason(); reason != "" {
		fmt.Fprintf(statement, ", unscheduled: %s", reason)
	}
	for _, container := range pod.GetContainers() {
		fmt.Fprintf(statement, "; container %s restarted %d times",
			container.GetName(), container.GetRestartCount())
		if waiting := container.GetState().GetWaitingReason(); waiting != "" {
			fmt.Fprintf(statement, ", waiting: %s", waiting)
		}
		// The current state's termination if it has one, otherwise the last observed. "Exited with
		// code 1 nine times" is a claim an engineer can act on; "the pod is not ready" is a symptom,
		// and the reason this reaches down two levels is that the first is what gets acted on.
		terminated := container.GetState().GetTermination()
		if terminated == nil {
			terminated = container.GetLastTermination()
		}
		if terminated != nil {
			fmt.Fprintf(statement, ", last exited with code %d (%s)",
				terminated.GetExitCode(), terminated.GetReason())
		}
	}
	return truncate(statement.String(), MaxEvidenceStatementBytes)
}

// interpretEvents groups what the cluster said. One item per distinct object and reason rather
// than one per event: that is how the cluster itself aggregates them, the count is the event's own
// rather than one this code derived, and a case does not acquire two hundred items that say the
// same thing.
func interpretEvents(
	request Request, result *relayv1.KubernetesNamespaceEventsResultV1,
	scope Scope, window Window, connection uuid.UUID, masking Masking,
) Validated {
	if result == nil {
		return Validated{Gaps: []Gap{unreadable(request)}}
	}

	// What was actually searched, in the customer's own terms. A subject naming the capability
	// would describe the mechanism rather than the scope, and it is the scope a reader has to
	// judge an absence against.
	subject := "events in namespace " + scope.Namespace
	switch result.GetOutcome() {
	case relayv1.KubernetesEventsOutcome_KUBERNETES_EVENTS_OUTCOME_UNREACHABLE:
		return Validated{Gaps: []Gap{{
			Cause: GapSourceUnreachable, CapabilityID: request.CapabilityID, Subject: subject,
			Consequence: "the cluster did not answer, so nothing it recorded about this window " +
				"is available",
		}}}
	case relayv1.KubernetesEventsOutcome_KUBERNETES_EVENTS_OUTCOME_UNAUTHORIZED:
		return Validated{Gaps: []Gap{{
			Cause: GapAuthorizationDenied, CapabilityID: request.CapabilityID, Subject: subject,
			Consequence: "the read was refused by the cluster's own authorization; an empty " +
				"answer here would have been an RBAC misconfiguration reported as a quiet cluster",
		}}}
	case relayv1.KubernetesEventsOutcome_KUBERNETES_EVENTS_OUTCOME_SUCCESS:
	default:
		return Validated{Gaps: []Gap{unreadable(request)}}
	}

	certificate := &Certificate{
		SearchedScope:      subject,
		PaginationComplete: result.GetComplete(),
		FullyAuthorized:    true,
		SourceFreshness:    result.GetAppliedRetentionHorizon().AsDuration(),
		MaskedFields:       masking.Fields(),
		AttestedBy:         TrustRelayAttested,
	}

	validated := Validated{}
	for _, group := range groupEvents(result.GetEvents()) {
		// What the brief calls a recent change. The vocabulary is hard-coded and narrow, and it is
		// on the written list of what this slice hard-codes rather than presented as a general
		// notion of change: the change ledger (ADR-010) is a slice of its own, and this is the
		// live approximation that will make the case for it.
		if changeReasons[group.reason] {
			validated.Changes = append(validated.Changes, BriefChange{
				At:      group.lastSeen,
				Summary: group.statement(),
				Item:    len(validated.Items),
			})
		}
		validated.Items = append(validated.Items, Item{
			RequestID:         request.ID,
			CapabilityID:      request.CapabilityID,
			CapabilityVersion: request.CapabilityVersion,
			Connection:        connection,
			Statement:         group.statement(),
			Content:           truncate(group.messages(), MaxEvidenceContentBytes),
			Trust:             TrustRelayAttested,
			Certificate:       certificate,
			SourceObservedAt:  group.lastSeen,
			ReceivedAt:        time.Now(),
		})
	}

	// A complete read that found nothing is a certified negative and is usable. An incomplete one
	// is a gap, because a read that stopped at a bound cannot say the window was quiet — and so is
	// one whose scope was partly masked, because a hole the platform was not allowed to look into
	// is not a place it can report nothing was found.
	if len(validated.Items) == 0 && certificate.Certifies() &&
		!result.GetWindowPredatesRetention() {
		validated.Items = append(validated.Items, Item{
			RequestID:         request.ID,
			CapabilityID:      request.CapabilityID,
			CapabilityVersion: request.CapabilityVersion,
			Connection:        connection,
			Statement: "the cluster recorded no events in this namespace inside the window, and " +
				"the read completed",
			Absence:          true,
			Trust:            TrustRelayAttested,
			Certificate:      certificate,
			SourceObservedAt: timeOf(result.GetReadAt()),
			ReceivedAt:       time.Now(),
		})
	}

	// The honest half of reading changes live rather than from a change ledger: where the cluster's
	// own retention cannot reach back to where the engineer asked, the brief says so.
	if result.GetWindowPredatesRetention() {
		horizon := result.GetAppliedRetentionHorizon().AsDuration()
		if horizon == 0 {
			horizon = eventHorizon
		}
		validated.Gaps = append(validated.Gaps, Gap{
			Cause: GapRetentionHorizon, CapabilityID: request.CapabilityID,
			Subject: "events before " + window.Start.Add(window.Duration()-horizon).UTC().Format(time.RFC3339),
			Consequence: fmt.Sprintf(
				"the cluster keeps events for about %s and the window asked for %s, so what "+
					"happened at the start of it cannot be reconstructed from this source",
				horizon, window.Duration()),
		})
	}
	if !result.GetComplete() {
		validated.Gaps = append(validated.Gaps, Gap{
			Cause: GapResultTruncated, CapabilityID: request.CapabilityID, Subject: subject,
			Consequence: fmt.Sprintf(
				"%d events were returned under a bound of %d, so a quiet period in this window "+
					"cannot be distinguished from one the read did not reach",
				result.GetReturnedEventCount(), result.GetAppliedMaxEvents()),
		})
	}
	return validated
}

// changeReasons is what this slice treats as "something changed around the resource".
//
// It is a hard-coded list of Kubernetes event reasons and it is deliberately narrow. In this slice
// recent changes are a LIVE read rather than the persisted change ledger, so what is knowable is
// what the cluster still remembers: ReplicaSet history bounded by revisionHistoryLimit, and events
// bounded by the cluster's own TTL. Where that horizon truncates the window an engineer asked for,
// the brief records a CoverageGap saying so — which is the honest version, and is also the argument
// the change-ledger slice will be built on.
var changeReasons = map[string]bool{
	"ScalingReplicaSet": true,
	"SuccessfulCreate":  true,
	"SuccessfulDelete":  true,
	"Created":           true,
	"Started":           true,
	"Killing":           true,
	"Pulled":            true,
	"Preempted":         true,
	"Evicted":           true,
}

// eventGroup is one object and one reason, as the cluster aggregates them.
type eventGroup struct {
	object    string
	reason    string
	kind      string
	count     int32
	firstSeen time.Time
	lastSeen  time.Time
	texts     []string
}

func (g eventGroup) statement() string {
	occurrences := "once"
	if g.count > 1 {
		occurrences = strconv.Itoa(int(g.count)) + " times"
	}
	return truncate(fmt.Sprintf("%s %s for %s, %s between %s and %s",
		strings.ToLower(g.kind), g.reason, g.object, occurrences,
		g.firstSeen.UTC().Format(time.RFC3339), g.lastSeen.UTC().Format(time.RFC3339)),
		MaxEvidenceStatementBytes)
}

func (g eventGroup) messages() string { return strings.Join(g.texts, "\n") }

// groupEvents collapses events by object and reason, keeping the order they were first seen in so
// two runs over the same cluster produce the same evidence in the same order.
func groupEvents(events []*relayv1.KubernetesEvent) []eventGroup {
	order := make([]string, 0, len(events))
	grouped := make(map[string]*eventGroup, len(events))

	for _, event := range events {
		involved := event.GetInvolvedObject()
		object := involved.GetKind() + "/" + involved.GetName()
		key := object + " " + event.GetReason()

		group, seen := grouped[key]
		if !seen {
			group = &eventGroup{
				object:    object,
				reason:    event.GetReason(),
				kind:      event.GetType(),
				firstSeen: timeOf(event.GetFirstSeenAt()),
			}
			grouped[key] = group
			order = append(order, key)
		}
		group.count += max(event.GetCount(), 1)
		if at := timeOf(event.GetLastSeenAt()); at.After(group.lastSeen) {
			group.lastSeen = at
		}
		if message := event.GetMessage(); message != "" && len(group.texts) < maxContentLines {
			group.texts = append(group.texts, message)
		}
	}

	collapsed := make([]eventGroup, 0, len(order))
	for _, key := range order {
		collapsed = append(collapsed, *grouped[key])
	}
	return collapsed
}

// interpretLogs reads what a container said. This is the artifact ADR-012 exists to permit: the
// actual error string, which cannot be enumerated in advance and is the thing that explains the
// incident.
func interpretLogs(
	request Request, result *relayv1.KubernetesContainerLogsResultV1,
	scope Scope, connection uuid.UUID, masking Masking,
) Validated {
	if result == nil {
		return Validated{Gaps: []Gap{unreadable(request)}}
	}

	subject := "container logs in namespace " + scope.Namespace
	switch result.GetOutcome() {
	case relayv1.KubernetesLogsOutcome_KUBERNETES_LOGS_OUTCOME_UNREACHABLE:
		return Validated{Gaps: []Gap{{
			Cause: GapSourceUnreachable, CapabilityID: request.CapabilityID, Subject: subject,
			Consequence: "the cluster did not answer, so what the container said is unknown",
		}}}
	case relayv1.KubernetesLogsOutcome_KUBERNETES_LOGS_OUTCOME_UNAUTHORIZED:
		return Validated{Gaps: []Gap{{
			Cause: GapAuthorizationDenied, CapabilityID: request.CapabilityID, Subject: subject,
			Consequence: "the read was refused by the cluster's own authorization; an unauthorized " +
				"read is never reported as an empty log",
		}}}
	case relayv1.KubernetesLogsOutcome_KUBERNETES_LOGS_OUTCOME_POD_NOT_FOUND,
		relayv1.KubernetesLogsOutcome_KUBERNETES_LOGS_OUTCOME_CONTAINER_NOT_FOUND,
		relayv1.KubernetesLogsOutcome_KUBERNETES_LOGS_OUTCOME_PREVIOUS_CONTAINER_NOT_FOUND:
		return Validated{Gaps: []Gap{{
			Cause: GapTargetNotFound, CapabilityID: request.CapabilityID, Subject: subject,
			Consequence: "the container instance asked for is no longer on the node, so what it " +
				"said before it stopped is not recoverable from this source",
		}}}
	case relayv1.KubernetesLogsOutcome_KUBERNETES_LOGS_OUTCOME_SUCCESS:
	default:
		return Validated{Gaps: []Gap{unreadable(request)}}
	}

	truncated := result.GetTruncatedByLineBound() || result.GetTruncatedByByteBound()
	certificate := &Certificate{
		SearchedScope:      subject,
		PaginationComplete: result.GetComplete() && !truncated,
		FullyAuthorized:    true,
		MaskedFields:       masking.Fields(),
		AttestedBy:         TrustRelayAttested,
	}

	validated := Validated{}
	lines := result.GetLines()
	if len(lines) > 0 {
		texts := make([]string, 0, min(len(lines), maxContentLines))
		for _, line := range lines[:min(len(lines), maxContentLines)] {
			texts = append(texts, line.GetContent())
		}
		validated.Items = append(validated.Items, Item{
			RequestID:         request.ID,
			CapabilityID:      request.CapabilityID,
			CapabilityVersion: request.CapabilityVersion,
			Connection:        connection,
			Statement: fmt.Sprintf("the container returned %d log lines (%d bytes)",
				result.GetReturnedLineCount(), result.GetReturnedByteCount()),
			Content:          truncate(strings.Join(texts, "\n"), MaxEvidenceContentBytes),
			Trust:            TrustRelayAttested,
			Certificate:      certificate,
			SourceObservedAt: timeOf(lines[len(lines)-1].GetAt()),
			ReceivedAt:       time.Now(),
		})
	} else if certificate.Certifies() {
		validated.Items = append(validated.Items, Item{
			RequestID:         request.ID,
			CapabilityID:      request.CapabilityID,
			CapabilityVersion: request.CapabilityVersion,
			Connection:        connection,
			Statement:         "the container wrote nothing, and the read completed",
			Absence:           true,
			Trust:             TrustRelayAttested,
			Certificate:       certificate,
			SourceObservedAt:  timeOf(result.GetReadAt()),
			ReceivedAt:        time.Now(),
		})
	}

	// Bytes the Relay read and then dropped to stay inside the effective bounds. This is a BOUND
	// binding, not masking: the two were conflated here before the redaction report existed, which
	// told an operator their privacy policy had withheld evidence when what had actually happened
	// was that they asked for fewer bytes than the container had written. Masking now arrives as
	// its own report and its own gap; this is the one it used to be mistaken for.
	if withheld := result.GetWithheldByteCount(); withheld > 0 {
		validated.Gaps = append(validated.Gaps, Gap{
			Cause: GapResultTruncated, CapabilityID: request.CapabilityID, Subject: subject,
			Consequence: fmt.Sprintf(
				"%d bytes the read obtained were dropped to stay inside its byte bound, so what "+
					"they said is not part of this investigation and nothing here supports a claim "+
					"that the container never said something",
				withheld),
		})
	}
	if truncated {
		validated.Gaps = append(validated.Gaps, Gap{
			Cause: GapResultTruncated, CapabilityID: request.CapabilityID, Subject: subject,
			Consequence: fmt.Sprintf(
				"the log was cut at %d lines or %d bytes, so nothing here supports a claim that "+
					"the container never said something",
				result.GetAppliedMaxLines(), result.GetAppliedMaxBytes()),
		})
	}
	return validated
}

func unreadable(request Request) Gap {
	return Gap{
		Cause:        GapSourceUnreachable,
		CapabilityID: request.CapabilityID,
		Subject:      request.CapabilityID,
		Consequence: "the answer did not carry the payload this read expects, so nothing it may " +
			"have contained can be used",
	}
}

// timeOf reads a protocol timestamp, leaving the zero time where the source gave none. An item
// with no defensible time is listed beside the timeline rather than placed on it.
func timeOf(stamp interface{ AsTime() time.Time }) time.Time {
	if stamp == nil {
		return time.Time{}
	}
	at := stamp.AsTime()
	if at.Unix() <= 0 {
		return time.Time{}
	}
	return at
}

// truncate bounds a string at a byte ceiling without splitting a rune.
func truncate(value string, ceiling int) string {
	if len(value) <= ceiling {
		return value
	}
	cut := ceiling
	for cut > 0 && !utf8Start(value[cut]) {
		cut--
	}
	return value[:cut]
}

// utf8Start reports whether a byte begins a rune, so a truncation cannot leave half of one.
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
