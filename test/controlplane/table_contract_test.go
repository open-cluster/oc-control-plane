package controlplane

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"testing"
)

func TestEveryListOperationUsesOneQueryContract(t *testing.T) {
	plane := startIntegrationPlane(t)
	root := "http://" + plane.operator + "/api/v1"
	base := plane.base(surfaceOrg)
	registration := plane.relay.registration.String()
	incidentID := plane.openIncident(t, "Listing contract", "listing-contract")
	listings := map[string]struct {
		endpoint string
		rows     string
	}{
		"organizations":         {root + "/organizations", "organizations"},
		"permissions":           {base + "/permissions", "permissions"},
		"members":               {base + "/members", "members"},
		"sessions":              {base + "/sessions", "sessions"},
		"audit events":          {base + "/audit-events", "events"},
		"integration types":     {base + "/integration-types", "types"},
		"integrations":          {base + "/integrations", "items"},
		"relays":                {base + "/relays", "items"},
		"relay integrations":    {base + "/relays/" + registration + "/integrations", "items"},
		"relay failures":        {base + "/relays/" + registration + "/failures", "items"},
		"relay conflicts":       {base + "/relays/" + registration + "/session-conflicts", "events"},
		"incidents":             {base + "/incidents", "items"},
		"incident alert events": {base + "/incidents/" + incidentID + "/alert-events", "items"},
		"investigations":        {base + "/investigations", "items"},
		"conversations":         {base + "/conversations", "items"},
		"webhook deliveries":    {base + "/webhook-deliveries", "items"},
	}

	for name, listing := range listings {
		t.Run(name, func(t *testing.T) {
			endpoint := listing.endpoint
			status, body := plane.call(t, http.MethodGet, endpoint, nil)
			if status != http.StatusOK {
				t.Fatalf("%s = %d, want 200: %s", endpoint, status, body)
			}
			want, _ := listPage(t, body, listing.rows)
			var got []string
			next := ""
			for pages := 0; ; pages++ {
				pageURL := endpoint + "?limit=1"
				if next != "" {
					pageURL += "&cursor=" + url.QueryEscape(next)
				}
				status, body = plane.call(t, http.MethodGet, pageURL, nil)
				if status != http.StatusOK {
					t.Fatalf("%s = %d, want 200: %s", pageURL, status, body)
				}
				rows, continuation := listPage(t, body, listing.rows)
				got = append(got, rows...)
				if continuation == "" {
					break
				}
				if continuation == next || pages > len(want) {
					t.Fatalf("cursor did not advance")
				}
				next = continuation
			}
			if !slices.Equal(got, want) {
				t.Errorf("paged rows differ from the default order")
			}
			for _, query := range []string{
				"limit=bad",
				"limit=201",
				"limit=1&limit=2",
				"cursor=tampered",
				"notAFilter=value",
			} {
				status, body = plane.call(t, http.MethodGet, endpoint+"?"+query, nil)
				if status != http.StatusBadRequest {
					t.Errorf("%s?%s = %d, want 400: %s", endpoint, query, status, body)
				}
			}
		})
	}
}

func listPage(t *testing.T, body, field string) ([]string, string) {
	t.Helper()
	var document map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &document); err != nil {
		t.Fatalf("decoding list: %v", err)
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(document[field], &raw); err != nil {
		t.Fatalf("decoding %s: %v", field, err)
	}
	rows := make([]string, 0, len(raw))
	for _, row := range raw {
		rows = append(rows, listRowKey(row))
	}
	var next string
	if value := document["next"]; len(value) != 0 && string(value) != "null" {
		if err := json.Unmarshal(value, &next); err != nil {
			t.Fatalf("decoding next: %v", err)
		}
	}
	return rows, next
}

func listRowKey(raw json.RawMessage) string {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) == nil {
		for _, key := range []string{"id", "registrationId", "key", "jobId", "sequence"} {
			if value := object[key]; len(value) != 0 {
				return key + ":" + string(value)
			}
		}
	}
	return string(raw)
}

func TestListFilterAndSortValuesAreStrict(t *testing.T) {
	plane := startIntegrationPlane(t)
	base := plane.base(surfaceOrg)
	cases := map[string]string{
		"integration boolean":     base + "/integrations?disabled=foo",
		"integration type":        base + "/integrations?type=unknown",
		"integration relay":       base + "/integrations?relay=bad",
		"integration plus sort":   base + "/integrations?sort=%2BcreatedAt",
		"relay state":             base + "/relays?state=unknown",
		"relay plus sort":         base + "/relays?sort=%2BregisteredAt",
		"incident status":         base + "/incidents?status=unknown",
		"incident integration":    base + "/incidents?integrationId=bad",
		"incident plus sort":      base + "/incidents?sort=%2BlastSeenAt",
		"investigation incident":  base + "/investigations?incidentId=bad",
		"conversation state":      base + "/conversations?state=unknown",
		"conversation incident":   base + "/conversations?incidentId=bad",
		"conversation plus sort":  base + "/conversations?sort=%2BlastActivityAt",
		"webhook delivery status": base + "/webhook-deliveries?status=unknown",
	}
	for name, endpoint := range cases {
		t.Run(name, func(t *testing.T) {
			status, body := plane.call(t, http.MethodGet, endpoint, nil)
			if status != http.StatusBadRequest {
				t.Errorf("%s = %d, want 400: %s", endpoint, status, body)
			}
		})
	}
}

func TestCursorCannotMoveBetweenListOperations(t *testing.T) {
	plane := startIntegrationPlane(t)
	base := plane.base(surfaceOrg)
	plane.createAlertmanager(t, "Cursor scope")

	status, body := plane.call(t, http.MethodGet, base+"/audit-events?limit=1", nil)
	if status != http.StatusOK {
		t.Fatalf("listing audit events = %d: %s", status, body)
	}
	_, cursor := listPage(t, body, "events")
	if cursor == "" {
		t.Fatal("audit listing did not produce a cursor")
	}
	status, body = plane.call(t, http.MethodGet,
		base+"/members?cursor="+url.QueryEscape(cursor), nil)
	if status != http.StatusBadRequest {
		t.Errorf("cross-list cursor = %d, want 400: %s", status, body)
	}
}

func TestListEnvelopeIsStable(t *testing.T) {
	plane := startIntegrationPlane(t)
	base := plane.base(surfaceOrg)

	plane.createAlertmanager(t, "Contract Alertmanager")

	listings := map[string]string{
		"integrations":   base + "/integrations",
		"relays":         base + "/relays",
		"incidents":      base + "/incidents",
		"investigations": base + "/investigations",
		"relay integrations": base + "/relays/" + plane.relay.registration.String() +
			"/integrations",
	}

	for name, url := range listings {
		t.Run(name, func(t *testing.T) {
			status, body := plane.call(t, http.MethodGet, url, nil)
			if status != http.StatusOK {
				t.Fatalf("%s = %d: %s", url, status, body)
			}
			var envelope map[string]json.RawMessage
			decodeInto(t, body, &envelope)

			for _, key := range []string{"items", "next", "total", "partial"} {
				if _, present := envelope[key]; !present {
					t.Errorf("%s answered without %q: %s", name, key, body)
				}
			}
			if string(envelope["items"]) == "null" {
				t.Errorf("%s answered items as null rather than []", name)
			}
			if string(envelope["partial"]) == "null" {
				t.Errorf("%s answered partial as null rather than []", name)
			}
		})
	}
}
