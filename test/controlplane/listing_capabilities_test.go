package controlplane

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestIntegrationListingAppliesDocumentedCapabilities(t *testing.T) {
	plane := startIntegrationPlane(t)
	base := plane.base(surfaceOrg)
	alpha := plane.createAlertmanager(t, "Alpha receiver")
	zulu := plane.createAlertmanager(t, "Zulu receiver")

	status, body := plane.call(t, http.MethodPost, base+"/integrations", map[string]any{
		"type": "kubernetes", "name": "Cluster runtime",
		"relayId": plane.relay.registration.String(),
	})
	if status != http.StatusCreated {
		t.Fatalf("creating relay integration = %d: %s", status, body)
	}
	status, body = plane.call(t, http.MethodPost,
		base+"/integrations/"+zulu.Integration.ID+"/disable", nil)
	if status != http.StatusNoContent {
		t.Fatalf("disabling integration = %d: %s", status, body)
	}

	list := func(query string) []integrationBody {
		t.Helper()
		status, body := plane.call(t, http.MethodGet, base+"/integrations"+query, nil)
		if status != http.StatusOK {
			t.Fatalf("listing %s = %d: %s", query, status, body)
		}
		var page struct {
			Items []integrationBody `json:"items"`
		}
		decodeInto(t, body, &page)
		return page.Items
	}

	if items := list("?search=Alpha"); len(items) != 1 || items[0].ID != alpha.Integration.ID {
		t.Errorf("search returned %+v", items)
	}
	for _, item := range list("?type=kubernetes") {
		if item.Type != "kubernetes" {
			t.Errorf("type filter returned %q", item.Type)
		}
	}
	for _, item := range list("?relay=" + plane.relay.registration.String()) {
		if item.RelayID != plane.relay.registration.String() {
			t.Errorf("relay filter returned relay %q", item.RelayID)
		}
	}
	disabled := list("?disabled=true")
	if len(disabled) != 1 || disabled[0].ID != zulu.Integration.ID {
		t.Errorf("disabled filter returned %+v", disabled)
	}

	ascending := list("?sort=createdAt")
	descending := list("?sort=-createdAt")
	if len(ascending) < 2 || len(ascending) != len(descending) ||
		ascending[0].ID != descending[len(descending)-1].ID {
		t.Errorf("ascending=%v descending=%v", integrationIDs(ascending), integrationIDs(descending))
	}
}

func TestIncidentListingAppliesDocumentedCapabilities(t *testing.T) {
	plane := startIncidents(t)
	began := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	alphaKey := "group-alpha-token"
	zuluKey := "group-zulu-token"
	for _, payload := range []string{
		grouped(alphaKey, "alpha-1", "AlphaFailure", began),
		grouped(alphaKey, "alpha-2", "AlphaFailure", began.Add(time.Minute)),
		grouped(zuluKey, "zulu-1", "ZuluFailure", began.Add(2*time.Minute)),
		groupedResolution(zuluKey, "zulu-1", "ZuluFailure",
			began.Add(2*time.Minute), began.Add(3*time.Minute)),
	} {
		if status := plane.deliver(t, payload); status != http.StatusAccepted {
			t.Fatalf("seeding incident = %d", status)
		}
	}

	if items := plane.incidents(t, "?search=ALPHA").Items; len(items) != 1 || items[0].Title != "AlphaFailure" {
		t.Errorf("title search returned %+v", items)
	}
	if items := plane.incidents(t, "?search=group-zulu-token").Items; len(items) != 1 || items[0].Grouping.Key != zuluKey {
		t.Errorf("grouping-key search returned %+v", items)
	}
	if items := plane.incidents(t, "?status=resolved").Items; len(items) != 1 || items[0].Status != "resolved" {
		t.Errorf("status filter returned %+v", items)
	}
	if items := plane.incidents(t, "?integrationId="+uuid.NewString()).Items; len(items) != 0 {
		t.Errorf("integration filter returned %+v", items)
	}

	for _, field := range []string{"lastSeenAt", "firstSeenAt", "title", "alertEventCount"} {
		ascending := plane.incidents(t, "?sort="+field).Items
		descending := plane.incidents(t, "?sort=-"+field).Items
		if len(ascending) != 2 || len(descending) != 2 || ascending[0].ID != descending[1].ID {
			t.Errorf("sort %s did not reverse the rows", field)
		}
	}
}

func TestConversationAndInvestigationListingsApplyDocumentedCapabilities(t *testing.T) {
	plane := startIntegrationPlane(t)
	firstIncident := plane.openIncident(t, "Checkout unavailable", "checkout-unavailable")
	secondIncident := plane.openIncident(t, "Payments unavailable", "payments-unavailable")

	open := func(subject, incidentID string) string {
		t.Helper()
		status, body := plane.call(t, http.MethodPost, plane.base(surfaceOrg)+"/conversations",
			map[string]any{"subject": subject, "incidentId": incidentID, "message": "investigate"})
		if status != http.StatusCreated {
			t.Fatalf("opening conversation = %d: %s", status, body)
		}
		var created struct {
			ID string `json:"id"`
		}
		decodeInto(t, body, &created)
		return created.ID
	}
	firstConversation := open("Checkout follow-up", firstIncident)
	open("Payments follow-up", secondIncident)

	type conversationRow struct {
		ID         string `json:"id"`
		IncidentID string `json:"incidentId"`
		Subject    string `json:"subject"`
		State      string `json:"state"`
	}
	listConversations := func(query string) []conversationRow {
		t.Helper()
		status, body := plane.call(t, http.MethodGet,
			plane.base(surfaceOrg)+"/conversations"+query, nil)
		if status != http.StatusOK {
			t.Fatalf("listing conversations %s = %d: %s", query, status, body)
		}
		var page struct {
			Items []conversationRow `json:"items"`
		}
		decodeInto(t, body, &page)
		return page.Items
	}

	if rows := listConversations("?search=CHECKOUT"); len(rows) != 1 || rows[0].ID != firstConversation {
		t.Errorf("conversation search returned %+v", rows)
	}
	if rows := listConversations("?incidentId=" + firstIncident); len(rows) != 1 || rows[0].IncidentID != firstIncident {
		t.Errorf("conversation incident filter returned %+v", rows)
	}
	if rows := listConversations("?state=closed"); len(rows) != 0 {
		t.Errorf("conversation state filter returned %+v", rows)
	}
	ascending := listConversations("?sort=lastActivityAt")
	descending := listConversations("?sort=-lastActivityAt")
	if len(ascending) != 2 || len(descending) != 2 || ascending[0].ID != descending[1].ID {
		t.Errorf("conversation sort did not reverse the rows")
	}

	type investigationPage struct {
		Items []struct {
			IncidentID string `json:"incidentId"`
		} `json:"items"`
	}
	status, body := plane.call(t, http.MethodGet,
		plane.base(surfaceOrg)+"/investigations?incidentId="+firstIncident, nil)
	if status != http.StatusOK {
		t.Fatalf("filtering investigations = %d: %s", status, body)
	}
	var investigations investigationPage
	decodeInto(t, body, &investigations)
	if len(investigations.Items) == 0 {
		t.Fatal("investigation incident filter returned no rows")
	}
	for _, item := range investigations.Items {
		if item.IncidentID != firstIncident {
			t.Errorf("investigation incident filter returned %+v", investigations.Items)
		}
	}
}

func integrationIDs(items []integrationBody) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}
