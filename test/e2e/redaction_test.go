package e2e

import (
	"context"
	"strings"
	"testing"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"
)

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

	if !containsMarker(logs.GetLines(), keptAlongside) {
		t.Fatalf("the container's own words did not arrive at all: %+v", logs.GetLines())
	}
	if !containsMarker(logs.GetLines(), "[redacted:") {
		t.Errorf("nothing marks the masked span, so a reader cannot tell masking from absence: %+v",
			logs.GetLines())
	}

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

	for _, rule := range field.GetRuleIds() {
		if strings.Contains(rule, leakedSecret) {
			t.Fatal("the redaction report carries the value it reports having masked")
		}
	}

	surviving, err := h.truth.occurrencesOf(context.Background(), keptAlongside)
	if err != nil {
		t.Fatalf("sweeping the database: %v", err)
	}
	if len(surviving) == 0 {
		t.Error("nothing the container said survived to the database; masking removed the read " +
			"rather than the secret in it")
	}
}
