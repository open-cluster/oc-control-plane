package e2e

import (
	"context"
	"strings"
	"testing"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"
)

// The property that matters most about redaction and cannot be unit tested: a secret placed in a
// REAL container's log output does not appear anywhere in the control plane's durable state.
//
// The assertion is over the database rather than over a function's return value, because the
// claim is about what leaves the cluster. Everything between the container and that database is
// real here — a real kubelet's log, the real capability, the real enforcement point, the real
// wire, the real recording transaction — which is the only arrangement in which the claim means
// anything.
//
// The NEGATIVE half is the important one and it is written to fail if enforcement is removed. A
// test that only checked the marker was present would pass against an implementation that sent
// both the marker and the secret.

func TestProof_ASecretInAContainersLogDoesNotReachTheControlPlane(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	defer cancel()
	pod := h.awaitPod(t, ctx, leakingWorkload)

	record := h.awaitTerminal(t,
		h.dispatchLogs(t, fixtureNamespace, pod, leakingWorkload, false, 100, 65536))
	if record.Status != jobSucceeded {
		t.Fatalf("the logs job reached %s, want succeeded\n\n%s", record.Status, h.diagnostics())
	}

	logs := logsResultOf(t, record)
	if logs.GetOutcome() != relayv1.KubernetesLogsOutcome_KUBERNETES_LOGS_OUTCOME_SUCCESS {
		t.Fatalf("the log read reported %v, want SUCCESS", logs.GetOutcome())
	}

	// The line arrived, and what was NOT a secret on it survived. A rule that masked the whole
	// line would destroy evidence while passing an assertion that only looked for the secret's
	// absence.
	if !containsMarker(logs.GetLines(), keptAlongside) {
		t.Fatalf("the container's own words did not arrive at all: %+v", logs.GetLines())
	}
	if !containsMarker(logs.GetLines(), "[redacted:") {
		t.Errorf("nothing marks the masked span, so a reader cannot tell masking from absence: %+v",
			logs.GetLines())
	}

	// THE NEGATIVE ASSERTION. Every text and binary column of every table, swept for the secret.
	found, err := h.truth.occurrencesOf(context.Background(), leakedSecret)
	if err != nil {
		t.Fatalf("sweeping the database: %v", err)
	}
	if len(found) > 0 {
		t.Fatalf("the secret reached the control plane's durable state in %s.\n"+
			"Redaction is not standing between a customer's container and this database.",
			strings.Join(found, ", "))
	}
}

// Masking is REPORTED, and the report is what the control plane turns into a coverage gap. Counts
// and rule identifiers survive the wire; the value does not.
func TestProof_TheRedactionReportCrossesTheProtocolIntact(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	defer cancel()
	pod := h.awaitPod(t, ctx, leakingWorkload)

	record := h.awaitTerminal(t,
		h.dispatchLogs(t, fixtureNamespace, pod, leakingWorkload, false, 100, 65536))
	if record.Status != jobSucceeded {
		t.Fatalf("the logs job reached %s, want succeeded\n\n%s", record.Status, h.diagnostics())
	}

	report := recordedResultOf(t, record).GetRedaction()
	if report == nil || len(report.GetFields()) == 0 {
		t.Fatal("a result carrying a masked credential arrived with no redaction report, so " +
			"masking would be indistinguishable from absence")
	}

	field := report.GetFields()[0]
	if field.GetFieldName() != "kubernetes_container_logs_v1.lines.content" {
		t.Errorf("the report names %q, want the log content field", field.GetFieldName())
	}
	if field.GetMaskedOccurrenceCount() == 0 {
		t.Error("the report counts no masked occurrences")
	}
	if len(field.GetRuleIds()) == 0 {
		t.Error("the report names no rule, so nobody knows which rule to adjust")
	}

	// The report itself must not become a way to send what it says was withheld.
	for _, rule := range field.GetRuleIds() {
		if strings.Contains(rule, leakedSecret) {
			t.Fatal("the redaction report carries the value it reports having masked")
		}
	}

	// Everything that is NOT a secret is still in the durable record. Redaction removes a field's
	// secrets, not the read.
	surviving, err := h.truth.occurrencesOf(context.Background(), keptAlongside)
	if err != nil {
		t.Fatalf("sweeping the database: %v", err)
	}
	if len(surviving) == 0 {
		t.Error("nothing the container said survived to the database; masking removed the read " +
			"rather than the secret in it")
	}
}
