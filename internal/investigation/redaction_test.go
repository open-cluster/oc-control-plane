package investigation_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"

	"github.com/open-cluster/oc-control-plane/internal/capability"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

// What a masked field costs an investigation, and what it must never be mistaken for.

const maskedLogField = "kubernetes_container_logs_v1.lines.content"

func maskedLogsRead(t *testing.T, lines []*relayv1.KubernetesLogLine, masked uint32) investigation.Read {
	t.Helper()

	result := &relayv1.CapabilityResult{
		Result: &relayv1.CapabilityResult_KubernetesContainerLogsV1{
			KubernetesContainerLogsV1: &relayv1.KubernetesContainerLogsResultV1{
				Outcome:           relayv1.KubernetesLogsOutcome_KUBERNETES_LOGS_OUTCOME_SUCCESS,
				Lines:             lines,
				ReturnedLineCount: int32(len(lines)),
				Complete:          true,
				ReadAt:            timestamppb.New(time.Now().Add(-time.Minute)),
				AppliedMaxLines:   2000,
				AppliedMaxBytes:   262144,
			},
		},
	}
	if masked > 0 {
		result.Redaction = &relayv1.RedactionReport{Fields: []*relayv1.RedactedField{{
			FieldName:             maskedLogField,
			MaskedOccurrenceCount: masked,
			RuleIds:               []string{"builtin.json_web_token", "acme.internal_token"},
		}}}
	}

	encoded, err := proto.Marshal(result)
	if err != nil {
		t.Fatalf("encoding the result: %v", err)
	}
	return investigation.Read{Succeeded: true, Result: encoded}
}

func logsRequest() investigation.Request {
	return investigation.Request{
		ID:                uuid.New(),
		CapabilityID:      capability.KubernetesContainerLogs,
		CapabilityVersion: 1,
	}
}

func scopeAndWindow() (investigation.Scope, investigation.Window) {
	return investigation.Scope{
			Namespace: "shop", WorkloadName: "checkout",
		}, investigation.Window{
			Start: time.Now().Add(-time.Hour), End: time.Now().Add(time.Minute),
		}
}

func TestAMaskedFieldBecomesAGapNamingTheRuleAndTheCost(t *testing.T) {
	scope, window := scopeAndWindow()
	read := maskedLogsRead(t, []*relayv1.KubernetesLogLine{{
		At:      timestamppb.New(time.Now().Add(-2 * time.Minute)),
		Content: "connecting to [redacted:builtin.json_web_token]",
	}}, 2)

	validated := investigation.Interpret(logsRequest(), read, scope, window, uuid.New())

	var found *investigation.Gap
	for i, gap := range validated.Gaps {
		if gap.Cause == investigation.GapRedactionMasked {
			found = &validated.Gaps[i]
		}
	}
	if found == nil {
		t.Fatalf("masking produced no gap; gaps were %+v", validated.Gaps)
	}
	if found.Subject == maskedLogField {
		t.Errorf("the gap names the contract's field path rather than what a reader calls it: %q",
			found.Subject)
	}
	for _, expected := range []string{"2 values", "builtin.json_web_token", "acme.internal_token"} {
		if !strings.Contains(found.Consequence, expected) {
			t.Errorf("the consequence does not carry %q: %q", expected, found.Consequence)
		}
	}

	// The read still produced its evidence. Masking removes a field, not the read — a customer's
	// privacy policy must not cost them the whole answer.
	if len(validated.Items) == 0 {
		t.Error("a partially masked read produced no evidence at all")
	}
}

// The property story 13 asks for, at the seam where an absence is minted.
func TestAMaskedFieldPreventsACertifiedAbsence(t *testing.T) {
	scope, window := scopeAndWindow()

	// A read that returned no lines and completed. Without masking this is the certified negative
	// — "the container wrote nothing, and the read completed".
	clean := investigation.Interpret(logsRequest(), maskedLogsRead(t, nil, 0), scope, window, uuid.New())
	var absences int
	for _, item := range clean.Items {
		if item.Absence {
			absences++
		}
	}
	if absences != 1 {
		t.Fatalf("a complete empty read must mint exactly one certified absence, got %d", absences)
	}

	// The same read, with the customer's policy having masked something inside the scope. The
	// platform did not see part of what it is being asked to report nothing about.
	masked := investigation.Interpret(logsRequest(), maskedLogsRead(t, nil, 1), scope, window, uuid.New())
	for _, item := range masked.Items {
		if item.Absence {
			t.Fatal("a read whose scope was partly masked minted a certified absence; a hole the " +
				"platform was not allowed to look into is not a place it can report nothing was found")
		}
	}
	if !hasCause(masked.Gaps, investigation.GapRedactionMasked) {
		t.Errorf("the masked read produced no redaction gap: %+v", masked.Gaps)
	}
}

func TestACertificateDoesNotCertifyOverAMaskedField(t *testing.T) {
	certificate := &investigation.Certificate{
		SearchedScope: "container logs in namespace shop", PaginationComplete: true,
		FullyAuthorized: true, AttestedBy: investigation.TrustRelayAttested,
	}
	if !certificate.Certifies() {
		t.Fatal("a complete, fully authorized read must certify")
	}

	certificate.MaskedFields = []string{"the container's own log text"}
	if certificate.Certifies() {
		t.Fatal("a certificate over a scope containing a masked field must not certify")
	}
}

func TestValidateAbsenceRefusesACandidateThatNamesRedactions(t *testing.T) {
	_, window := scopeAndWindow()
	candidate := investigation.Candidate{
		Observation: investigation.Observation{
			Statement:  "the container wrote nothing, and the read completed",
			ReceivedAt: time.Now(),
		},
		CapabilityID:      capability.KubernetesContainerLogs,
		CapabilityVersion: 1,
		Connection:        uuid.New(),
		Trust:             investigation.TrustRelayAttested,
		Certificate: &investigation.Certificate{
			SearchedScope:      "container logs in namespace shop",
			PaginationComplete: true, FullyAuthorized: true,
			MaskedFields: []string{"the container's own log text"},
			AttestedBy:   investigation.TrustRelayAttested,
		},
	}

	if _, err := investigation.ValidateAbsence(candidate, window); err == nil {
		t.Fatal("an absence was admitted over a read whose field was masked")
	}

	candidate.Certificate.MaskedFields = nil
	if _, err := investigation.ValidateAbsence(candidate, window); err != nil {
		t.Fatalf("the same read without masking must be admissible: %v", err)
	}
}

// Bytes dropped to stay inside a bound are truncation, not masking. Reporting them as a
// redaction told an operator their privacy policy had withheld evidence when what had happened
// was that they asked for fewer bytes than the container had written.
func TestBytesDroppedByABoundAreTruncationAndNotMasking(t *testing.T) {
	scope, window := scopeAndWindow()

	result := &relayv1.CapabilityResult{
		Result: &relayv1.CapabilityResult_KubernetesContainerLogsV1{
			KubernetesContainerLogsV1: &relayv1.KubernetesContainerLogsResultV1{
				Outcome: relayv1.KubernetesLogsOutcome_KUBERNETES_LOGS_OUTCOME_SUCCESS,
				Lines: []*relayv1.KubernetesLogLine{{
					At: timestamppb.New(time.Now().Add(-time.Minute)), Content: "the last line",
				}},
				ReturnedLineCount: 1, Complete: true, WithheldByteCount: 412,
				ReadAt: timestamppb.New(time.Now()), AppliedMaxLines: 10, AppliedMaxBytes: 1024,
			},
		},
	}
	encoded, err := proto.Marshal(result)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}

	validated := investigation.Interpret(
		logsRequest(), investigation.Read{Succeeded: true, Result: encoded},
		scope, window, uuid.New())

	if hasCause(validated.Gaps, investigation.GapRedactionMasked) {
		t.Error("a bound binding was reported as this organization's redaction policy")
	}
	if !hasCause(validated.Gaps, investigation.GapResultTruncated) {
		t.Errorf("dropped bytes must be reported as truncation: %+v", validated.Gaps)
	}
}

func hasCause(gaps []investigation.Gap, cause investigation.GapCause) bool {
	for _, gap := range gaps {
		if gap.Cause == cause {
			return true
		}
	}
	return false
}
