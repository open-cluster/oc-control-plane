package controlplane

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/postmortem"
)

func TestResolvedIncidentPostmortemDraftCanBeCorrectedRegeneratedAndReviewed(t *testing.T) {
	plane := startIncidents(t)
	started := time.Now().UTC().Add(-time.Hour)
	ended := started.Add(20 * time.Minute)
	key := `{}:{alertname="CheckoutUnavailable"}`
	if status := plane.deliver(t, grouped(key, "postmortem-fp", "CheckoutUnavailable", started)); status != http.StatusAccepted {
		t.Fatalf("firing delivery = %d", status)
	}
	if status := plane.deliver(t, groupedResolution(key, "postmortem-fp",
		"CheckoutUnavailable", started, ended)); status != http.StatusAccepted {
		t.Fatalf("resolution delivery = %d", status)
	}
	incidents := plane.incidents(t, "?status=resolved")
	if len(incidents.Items) != 1 || !incidents.Items[0].PostmortemEligible {
		t.Fatalf("resolved incidents = %+v", incidents.Items)
	}
	base := "/api/v1/organizations/" + intakeOrganization + "/incidents/" +
		incidents.Items[0].ID + "/postmortem"

	status, body := plane.call(t, http.MethodPost, base, nil)
	if status != http.StatusCreated {
		t.Fatalf("generate answered %d: %s", status, body)
	}
	var draft postmortem.Postmortem
	if err := json.Unmarshal([]byte(body), &draft); err != nil {
		t.Fatal(err)
	}
	if draft.Status != postmortem.StatusDraft || draft.Impact != postmortem.NeedsHumanInput {
		t.Fatalf("draft = %+v", draft)
	}

	status, body = plane.call(t, http.MethodPost, base+"/regenerate",
		map[string]any{"resolution": "Rolled back the deployment."})
	if status != http.StatusOK {
		t.Fatalf("regenerate answered %d: %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &draft); err != nil {
		t.Fatal(err)
	}
	if draft.Revision != 2 || draft.Resolution != "Rolled back the deployment." {
		t.Fatalf("regenerated draft = %+v", draft)
	}

	status, body = plane.call(t, http.MethodPatch, base,
		map[string]any{"impact": "A subset of checkout requests failed."})
	if status != http.StatusOK {
		t.Fatalf("correct answered %d: %s", status, body)
	}
	status, body = plane.call(t, http.MethodPost, base+"/review", nil)
	if status != http.StatusOK {
		t.Fatalf("review answered %d: %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &draft); err != nil {
		t.Fatal(err)
	}
	if draft.Status != postmortem.StatusReviewed || draft.ReviewedAt.IsZero() ||
		draft.Impact != "A subset of checkout requests failed." {
		t.Fatalf("reviewed draft = %+v", draft)
	}
}
