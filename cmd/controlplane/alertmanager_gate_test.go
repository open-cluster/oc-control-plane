package main

import (
	"bytes"
	"net/http"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/intake"
	"github.com/open-cluster/oc-control-plane/internal/storage"
)

// The compatibility gate for the front door.
//
// Every other test of intake posts a body this repository wrote in the shape it believes
// Alertmanager sends. That proves the parser handles what we believe, which is exactly the
// belief worth checking. Here a REAL Alertmanager is started, handed the configuration the
// documentation page gives customers, and told to fire an alert; the body under test is
// Alertmanager's own.
//
// The configuration is not written twice. It is read out of the page, and only the three
// values a customer substitutes are substituted. If somebody edits the page into something
// that does not work, or changes intake so the documented configuration no longer reaches
// it, this fails — which is the point, because the failure it guards against is a customer
// whose first alert vanished, and the symptom of that is silence.

// THE GATE. A real alert, fired through a real Alertmanager configured exactly as the
// documentation page says, becomes an incident an SRE can investigate — and closes when the
// alert does.
func TestAlertmanagerGate_TheDocumentedConfigurationDeliversAnInvestigableIncident(t *testing.T) {
	gate := startAlertmanagerGate(t)
	const alertname = "GateNodeNotReady"
	began := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)

	gate.fire(t, firingAlert(alertname, began))
	delivered := gate.recorder.await(t, alertname, "firing", 1)[0]

	if delivered.Status != http.StatusAccepted {
		t.Fatalf("intake answered alertmanager %d, want 202; the documented configuration "+
			"did not authenticate:\n%s", delivered.Status, delivered.Body)
	}
	if delivered.Token != gate.secret {
		t.Fatalf("alertmanager presented %q in %s, want the webhook secret; the documented "+
			"header configuration is wrong for %s",
			delivered.Token, intake.TokenHeader, alertmanagerGateImage)
	}

	// The Signal is what the alert became. Everything the customer's alerting already knew
	// has to survive intake, because it is what the investigation starts from.
	signals := gate.signalsNamed(t, alertname)
	if len(signals) != 1 {
		t.Fatalf("one fired alert produced %d signals, want 1", len(signals))
	}
	signal := signals[0]
	if signal.SourceKey != delivered.Payload.Alerts[0].Fingerprint {
		t.Errorf("the signal is keyed %q, want alertmanager's own fingerprint %q",
			signal.SourceKey, delivered.Payload.Alerts[0].Fingerprint)
	}
	if signal.Title != alertname {
		t.Errorf("the signal is titled %q, want %q", signal.Title, alertname)
	}
	if signal.Labels["severity"] != "critical" || signal.Labels["namespace"] != "payments" {
		t.Errorf("the alert's labels did not survive intake: %v", signal.Labels)
	}
	if signal.Annotations["runbook_url"] == "" || signal.Annotations["dashboard_url"] == "" {
		t.Errorf("the runbook and dashboard the operator wrote did not survive intake: %v",
			signal.Annotations)
	}
	if signal.GeneratorURL == "" {
		t.Error("the alert's generator url did not survive intake")
	}
	if !signal.StartedAt.Equal(began) {
		t.Errorf("the signal started at %s, want the alert's own %s", signal.StartedAt, began)
	}

	// The page tells an SRE to fire a test alert and select Verify. That is how they learn
	// their first alert arrived, so it is asserted against a real one.
	verifyStatus, verifyBody := gate.call(t, http.MethodPost,
		gate.base(surfaceOrg)+"/integrations/"+gate.integration+"/verify", nil)
	if verifyStatus != http.StatusOK {
		t.Fatalf("verifying = %d: %s", verifyStatus, verifyBody)
	}
	var verified integrationBody
	decodeInto(t, verifyBody, &verified)
	if verified.Status != "active" {
		t.Errorf("after a real alertmanager delivery the integration verifies as %q, want "+
			"active; note: %s", verified.Status, verified.VerifyNote)
	}

	// The incident, grouped on the identity ALERTMANAGER supplied. Nothing here infers it.
	episodeID := gate.episodeByTitle(t, alertname)
	episode := gate.episode(t, episodeID)
	if episode.Grouping.Basis != "source_grouping" {
		t.Errorf("the episode's grouping basis is %q, want source_grouping",
			episode.Grouping.Basis)
	}
	if episode.Grouping.Key != delivered.Payload.GroupKey {
		t.Errorf("the episode is grouped on %q, want alertmanager's own group key %q",
			episode.Grouping.Key, delivered.Payload.GroupKey)
	}
	if episode.Status != "open" {
		t.Errorf("the episode is %q while its alert fires, want open", episode.Status)
	}

	// An investigation must be openable against it, and must start from what the alert
	// carried: an incident born from an alert that begins with nothing throws away what the
	// operator's own alerting already knew.
	status, body := gate.call(t, http.MethodPost, gate.base(surfaceOrg)+"/investigations",
		map[string]any{"episodeId": episodeID})
	if status != http.StatusAccepted {
		t.Fatalf("opening an investigation on the episode = %d: %s", status, body)
	}
	var opened struct {
		ID string `json:"id"`
	}
	decodeInto(t, body, &opened)
	gate.awaitInvestigation(t, opened.ID)

	trigger := gate.investigator.orientation.Trigger
	if trigger == nil {
		t.Fatal("the investigation started with no trigger; an incident from an alert must " +
			"begin with what the alert said")
	}
	if trigger.Title != alertname {
		t.Errorf("the trigger is titled %q, want %q", trigger.Title, alertname)
	}
	if trigger.Labels["namespace"] != "payments" {
		t.Errorf("the trigger carries labels %v, want the alert's own", trigger.Labels)
	}
	if trigger.Annotations["runbook_url"] == "" || trigger.Annotations["dashboard_url"] == "" {
		t.Errorf("the trigger carries annotations %v, want the runbook and dashboard the "+
			"operator's alerting already knew", trigger.Annotations)
	}
	if trigger.GeneratorURL == "" {
		t.Error("the trigger carries no generator url, so the investigation cannot point " +
			"back at where the alert came from")
	}

	// A resolution closes the episode the firing opened rather than opening a second one.
	ended := time.Now().UTC().Truncate(time.Second)
	gate.fire(t, resolvedAlert(alertname, began, ended))
	gate.recorder.await(t, alertname, "resolved", 1)

	deadline := time.Now().Add(30 * time.Second)
	for {
		resolved := gate.episode(t, episodeID)
		if resolved.Status == "resolved" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the episode is still %q after alertmanager resolved its alert; an "+
				"incident list that never closes is one nobody trusts", resolved.Status)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// A network blip must not double an SRE's noise. Alertmanager retries a delivery it was told
// failed, and the retry carries the identical body — which intake has already accepted.
func TestAlertmanagerGate_ARetriedDeliveryCreatesNoSecondIncident(t *testing.T) {
	gate := startAlertmanagerGate(t)
	const alertname = "GateRetriedDelivery"
	began := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)

	// Intake will accept this one; Alertmanager will be told it did not.
	gate.recorder.failNextDelivery()
	gate.fire(t, firingAlert(alertname, began))
	delivered := gate.recorder.await(t, alertname, "firing", 2)

	if delivered[0].Status != http.StatusAccepted {
		t.Fatalf("the first delivery = %d, want 202", delivered[0].Status)
	}
	if delivered[1].Status != http.StatusOK {
		t.Errorf("alertmanager's retry of an identical body = %d, want 200 — a retrying "+
			"source has done nothing wrong and must be told to stop, not refused",
			delivered[1].Status)
	}
	if !bytes.Equal(delivered[0].Body, delivered[1].Body) {
		t.Fatal("alertmanager's retry carried a different body, so this asserts nothing " +
			"about redelivery")
	}

	// The same body once more, by hand: idempotence is a property of the body, not of who
	// happened to send it.
	if status := gate.replay(t, gate.secret, delivered[0].Body); status != http.StatusOK {
		t.Errorf("the same body redelivered = %d, want 200", status)
	}

	if signals := gate.signalsNamed(t, alertname); len(signals) != 1 {
		t.Errorf("one alert delivered three times produced %d signals, want 1", len(signals))
	}
	if list := gate.episodes(t); len(list.Items) != 1 {
		t.Errorf("one alert delivered three times produced %d episodes, want 1: %+v",
			len(list.Items), list.Items)
	}
	if accepted := gate.countDeliveries(t, storage.DeliveryAccepted, ""); accepted != 1 {
		t.Errorf("%d deliveries were accepted, want 1", accepted)
	}
	if duplicates := gate.countDeliveries(t, storage.DeliveryDuplicate, ""); duplicates != 2 {
		t.Errorf("%d deliveries were recorded as duplicates, want 2 — a retry that is not "+
			"recorded as a retry looks like a healthy source going quiet", duplicates)
	}
}

// Being turned away must never be indistinguishable from going quiet. Each refusal is its own
// recorded outcome, asserted against the body a real Alertmanager produced.
func TestAlertmanagerGate_ARefusedDeliveryIsRecordedRatherThanLost(t *testing.T) {
	gate := startAlertmanagerGate(t)
	const accepted = "GateAcceptedAlert"
	const afterDisabling = "GateAlertAfterDisabling"
	began := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)

	gate.fire(t, firingAlert(accepted, began))
	genuine := gate.recorder.await(t, accepted, "firing", 1)[0]
	if genuine.Status != http.StatusAccepted {
		t.Fatalf("the delivery this test refuses variants of = %d, want 202", genuine.Status)
	}

	// A wrong secret. The operator who rotated one wants to see it, not to wonder.
	status := gate.replay(t, "not-the-webhook-secret", genuine.Body)
	if status != http.StatusUnauthorized {
		t.Errorf("a real body with a wrong secret = %d, want 401", status)
	}

	// A mangled body — half of a real one, which is what middleware rewriting a delivery
	// looks like. This is the one body Alertmanager will not produce on request.
	status = gate.replay(t, gate.secret, genuine.Body[:len(genuine.Body)/2])
	if status != http.StatusBadRequest {
		t.Errorf("a truncated real body = %d, want 400", status)
	}

	// Turning the integration off has to mean something at the door.
	gate.setEnabled(t, false)
	gate.fire(t, firingAlert(afterDisabling, began))
	refused := gate.recorder.await(t, afterDisabling, "firing", 1)[0]
	if refused.Status != http.StatusUnauthorized {
		t.Errorf("a delivery to a disabled integration = %d, want 401 — an operator who "+
			"turned it off wants deliveries refused, not merely recorded", refused.Status)
	}
	if signals := gate.signalsNamed(t, afterDisabling); len(signals) != 0 {
		t.Errorf("a disabled integration recorded %d signals, want 0", len(signals))
	}

	// Three refusals, each saying why, next to the one acceptance.
	if count := gate.countDeliveries(t, storage.DeliveryAccepted, ""); count != 1 {
		t.Errorf("%d deliveries were accepted, want 1", count)
	}
	if count := gate.countDeliveries(t, storage.DeliveryRejected, storage.RefusedUnauthenticated); count != 2 {
		t.Errorf("%d deliveries were recorded as unauthenticated, want 2 (a wrong secret "+
			"and a disabled integration)", count)
	}
	if count := gate.countDeliveries(t, storage.DeliveryRejected, storage.RefusedMalformed); count != 1 {
		t.Errorf("%d deliveries were recorded as malformed, want 1", count)
	}
}
