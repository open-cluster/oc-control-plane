package controlplane

import (
	"net/http"
	"strings"

	"github.com/google/uuid"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

// Investigations at the composition seam: a real database, a fake vendor where Slack
// would be, and a scripted exchange at the model boundary. What is asserted is what
// an operator relies on: the tool universe the model is offered, subject inference and
// clarification, honest failure, refusal without a provider, and tenancy.

// openIncident creates an alertmanager integration and delivers one firing alert, so an
// open incident exists to investigate.
func (p *integrationPlane) openIncident(t *testing.T, alertname, fingerprint string) string {
	t.Helper()

	created := p.createAlertmanager(t, "Alertmanager for "+alertname)
	payload := []byte(`{
		"groupKey": "group-` + fingerprint + `",
		"alerts": [{
			"status": "firing",
			"fingerprint": "` + fingerprint + `",
			"labels": {"alertname": "` + alertname + `", "namespace": "payments"},
			"annotations": {"summary": "it broke"},
			"startsAt": "` + time.Now().UTC().Add(-30*time.Minute).Format(time.RFC3339) + `"
		}]
	}`)
	if status, body := p.deliver(t, created.Integration.ID, created.WebhookSecret,
		payload); status != http.StatusAccepted {
		t.Fatalf("the seeding delivery = %d: %s", status, body)
	}

	return p.incidentByTitle(t, alertname)
}

// episodeByTitle resolves the open incident a delivery just created.
func (p *integrationPlane) incidentByTitle(t *testing.T, title string) string {
	t.Helper()

	status, body := p.call(t, http.MethodGet, p.base(surfaceOrg)+"/incidents", nil)
	if status != http.StatusOK {
		t.Fatalf("listing incidents = %d: %s", status, body)
	}
	var listed struct {
		Items []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"items"`
	}
	decodeInto(t, body, &listed)
	for _, incident := range listed.Items {
		if incident.Title == title {
			return incident.ID
		}
	}
	t.Fatalf("no incident titled %s: %s", title, body)
	return ""
}

// awaitInvestigation polls until the investigation leaves its active lifecycle states.
func (p *integrationPlane) awaitInvestigation(t *testing.T, id string) string {
	t.Helper()
	return p.awaitInvestigationWithin(t, id, 30*time.Second)
}

func investigationActive(status string) bool {
	return status == "queued" || status == "investigating"
}

// The org-scoped tool universe is filtered by each Integration's verified grants before
// the model sees it: a pasted bot token — whose recorded grants can never include
// user_token — is offered the three grant-supported Slack tools and NOT
// slack.search_messages, while a user token with the search scope is offered all four.
// Asserted at the model boundary itself: the orientation's sources are exactly what the
// investigator may choose from, so a tool absent there is a capability the model was
// never offered.
func TestToolUniverseIsFilteredByVerifiedGrants(t *testing.T) {
	investigator := &scriptedInvestigatorMain{exchange: &scriptedExchangeMain{}}
	plane, vendor := autonomousPlaneWith(t, investigator, 0)

	if status, body := plane.createSlack(t, "Payments Bot Slack",
		"xoxb-good-token-1234"); status != http.StatusCreated {
		t.Fatalf("creating the bot-token slack = %d: %s", status, body)
	}
	vendor.accept("xoxp-user-token-5678")
	if status, body := plane.createSlack(t, "Payments User Slack",
		"xoxp-user-token-5678"); status != http.StatusCreated {
		t.Fatalf("creating the user-token slack = %d: %s", status, body)
	}

	incident := plane.openIncident(t, "DiskFull", "finger-grants")
	status, body := plane.call(t, http.MethodPost, plane.base(surfaceOrg)+"/investigations",
		map[string]any{"incidentId": incident})
	if status != http.StatusAccepted {
		t.Fatalf("opening = %d: %s", status, body)
	}
	var opened struct {
		ID string `json:"id"`
	}
	decodeInto(t, body, &opened)
	plane.awaitInvestigation(t, opened.ID)

	offered := map[string][]string{}
	for _, source := range investigator.orientation.Sources {
		var names []string
		for _, tool := range source.Tools {
			names = append(names, tool.Name)
		}
		offered[source.Integration.Name] = names
	}

	bot := offered["Payments Bot Slack"]
	if len(bot) != 3 {
		t.Fatalf("the bot token is offered %v, want the three grant-supported tools", bot)
	}
	for _, name := range bot {
		if name == "slack.search_messages" {
			t.Fatal("user-token-only search was offered to a bot-token integration")
		}
	}

	user := offered["Payments User Slack"]
	found := false
	for _, name := range user {
		found = found || name == "slack.search_messages"
	}
	if !found {
		t.Errorf("a user token granted search:read is offered %v; search is missing", user)
	}
}

func TestInvestigationFromAQuestionInfersTheSubject(t *testing.T) {
	investigator := &scriptedInvestigatorMain{exchange: &scriptedExchangeMain{}}
	plane, _ := autonomousPlaneWith(t, investigator, 0)
	base := plane.base(surfaceOrg)

	plane.openIncident(t, "DiskFull", "finger-question")

	status, body := plane.call(t, http.MethodPost, base+"/investigations",
		map[string]any{"question": "what is going on with DiskFull?"})
	if status != http.StatusAccepted {
		t.Fatalf("a question naming the one open incident = %d: %s", status, body)
	}
	var opened struct {
		ID       string `json:"id"`
		Subject  string `json:"subject"`
		Question string `json:"question"`
	}
	decodeInto(t, body, &opened)
	if opened.Subject != "DiskFull" {
		t.Errorf("subject = %q; the open incident is the organization context", opened.Subject)
	}
	if opened.Question == "" {
		t.Error("the operator's own words left the record")
	}
	plane.awaitInvestigation(t, opened.ID)
}

func TestAnAmbiguousQuestionGetsOneClarificationInPlainLanguage(t *testing.T) {
	plane, _ := autonomousPlaneWith(t, &scriptedInvestigatorMain{
		exchange: &scriptedExchangeMain{},
	}, 0)
	base := plane.base(surfaceOrg)

	plane.openIncident(t, "DiskFull", "finger-amb-1")
	plane.openIncident(t, "DiskAlmostFull", "finger-amb-2")

	status, body := plane.call(t, http.MethodPost, base+"/investigations",
		map[string]any{"question": "why is the disk broken?"})
	if status != http.StatusOK {
		t.Fatalf("an ambiguous question = %d, want 200 with a clarification: %s", status, body)
	}
	var answer struct {
		Clarification string `json:"clarification"`
	}
	decodeInto(t, body, &answer)
	if !strings.Contains(answer.Clarification, "DiskFull") ||
		!strings.Contains(answer.Clarification, "DiskAlmostFull") {
		t.Errorf("the clarification %q does not offer the candidates", answer.Clarification)
	}
	for _, banned := range []string{"scope", "evidence", "environment", "connection"} {
		if strings.Contains(strings.ToLower(answer.Clarification), banned) {
			t.Errorf("the clarification %q leaks internal vocabulary", answer.Clarification)
		}
	}

	status, listing := plane.call(t, http.MethodGet, base+"/investigations", nil)
	if status != http.StatusOK {
		t.Fatalf("listing = %d: %s", status, listing)
	}
	var opened struct {
		Items []struct {
			Question string `json:"question"`
		} `json:"items"`
	}
	decodeInto(t, listing, &opened)
	for _, item := range opened.Items {
		if item.Question == "why is the disk broken?" {
			t.Error("a clarification opened an investigation for the ambiguous question")
		}
	}
}

func TestAFailedReasonerFailsTheInvestigationHonestly(t *testing.T) {
	exchange := &scriptedExchangeMain{
		failure: investigation.ErrReasonerUnavailable,
	}
	plane, _ := autonomousPlaneWith(t,
		&scriptedInvestigatorMain{exchange: exchange}, 0)
	base := plane.base(surfaceOrg)

	incident := plane.openIncident(t, "DiskFull", "finger-fail")
	status, body := plane.call(t, http.MethodPost, base+"/investigations",
		map[string]any{"incidentId": incident})
	if status != http.StatusAccepted {
		t.Fatalf("opening = %d: %s", status, body)
	}
	var opened struct {
		ID string `json:"id"`
	}
	decodeInto(t, body, &opened)

	final := plane.awaitInvestigation(t, opened.ID)
	var read struct {
		Status   string `json:"status"`
		Error    string `json:"error"`
		Findings []any  `json:"findings"`
	}
	decodeInto(t, final, &read)
	if read.Status != "failed" || read.Error == "" {
		t.Errorf("a reasoner outage ends as %q with error %q; an investigation nobody "+
			"reasoned must never read as concluded", read.Status, read.Error)
	}
	if len(read.Findings) != 0 {
		t.Errorf("a failed investigation carries findings: %v", read.Findings)
	}
}

func TestInvestigationsRefuseWhenNoModelProviderIsConfigured(t *testing.T) {
	plane, _ := autonomousPlaneWith(t, nil, 0)
	base := plane.base(surfaceOrg)

	incident := plane.openIncident(t, "DiskFull", "finger-noprov")
	status, body := plane.call(t, http.MethodPost, base+"/investigations",
		map[string]any{"incidentId": incident})
	if status != http.StatusServiceUnavailable {
		t.Fatalf("opening with no provider = %d, want 503: %s", status, body)
	}
	if !strings.Contains(body, "model provider") {
		t.Errorf("the refusal %q does not say what the deployment lacks", body)
	}
}

func TestAnotherTenantSeesNoInvestigations(t *testing.T) {
	plane, _ := autonomousPlaneWith(t, &scriptedInvestigatorMain{
		exchange: &scriptedExchangeMain{},
	}, 0)

	incident := plane.openIncident(t, "DiskFull", "finger-tenant")
	status, body := plane.call(t, http.MethodPost,
		plane.base(surfaceOrg)+"/investigations", map[string]any{"incidentId": incident})
	if status != http.StatusAccepted {
		t.Fatalf("opening = %d: %s", status, body)
	}
	var opened struct {
		ID string `json:"id"`
	}
	decodeInto(t, body, &opened)

	status, answer := plane.call(t, http.MethodGet,
		plane.base(neighbourOrg)+"/investigations/"+opened.ID, nil)
	if status != http.StatusNotFound {
		t.Errorf("a neighbour reading this tenant's investigation = %d, want 404: %s",
			status, answer)
	}
}

// openInvestigation opens one from an incident and reports its id.
func (p *integrationPlane) openInvestigation(t *testing.T, base, incident string) string {
	t.Helper()

	status, body := p.call(t, http.MethodPost, base+"/investigations",
		map[string]any{"incidentId": incident})
	if status != http.StatusAccepted {
		t.Fatalf("opening an investigation for %s = %d: %s", incident, status, body)
	}
	var opened struct {
		ID string `json:"id"`
	}
	decodeInto(t, body, &opened)
	return opened.ID
}

// "EVERYTHING OPENED FROM THIS INCIDENT" had no answer over the API.
//
// The column exists and is indexed, and the listing did not accept it — so a console
// wanting the investigations for one incident had to read every investigation the tenant
// has and narrow them in a browser. That is the shape the frontend spec forbids, and it
// is forbidden for a reason: a filter applied after paging disagrees with itself on page
// two.
func TestInvestigationsCanBeListedByTheIncidentTheyCameFrom(t *testing.T) {
	investigator := &scriptedInvestigatorMain{exchange: &scriptedExchangeMain{}}
	plane, _ := autonomousPlaneWith(t, investigator, 0)
	base := plane.base(surfaceOrg)

	wanted := plane.openIncident(t, "DiskFull", "finger-by-incident-1")
	other := plane.openIncident(t, "HighLatency", "finger-by-incident-2")

	mine := plane.openInvestigation(t, base, wanted)
	theirs := plane.openInvestigation(t, base, other)
	plane.awaitInvestigation(t, mine)
	plane.awaitInvestigation(t, theirs)

	status, listing := plane.call(t, http.MethodGet,
		base+"/investigations?incidentId="+wanted, nil)
	if status != http.StatusOK {
		t.Fatalf("filtered listing = %d: %s", status, listing)
	}
	if !strings.Contains(listing, mine) {
		t.Errorf("the incident's own investigation is missing from its listing: %s", listing)
	}
	if strings.Contains(listing, theirs) {
		t.Errorf("another incident's investigation is in the listing: %s", listing)
	}

	// An incident this tenant does not have is an empty page, not an error and not a
	// disclosure: answering differently would let a caller probe for incident ids.
	status, empty := plane.call(t, http.MethodGet,
		base+"/investigations?incidentId="+uuid.NewString(), nil)
	if status != http.StatusOK {
		t.Fatalf("an unknown incident = %d, want an empty page: %s", status, empty)
	}
	if strings.Contains(empty, mine) || strings.Contains(empty, theirs) {
		t.Errorf("an unknown incident returned rows: %s", empty)
	}

	// A filter this listing does not serve is REFUSED rather than ignored. An ignored
	// filter returns everything while looking narrowed, which is the worse of the two.
	status, refused := plane.call(t, http.MethodGet,
		base+"/investigations?incident="+wanted, nil)
	if status != http.StatusBadRequest {
		t.Errorf("an unknown filter = %d, want 400: %s", status, refused)
	}

	// A value that is not an identifier is refused too, rather than reaching the database.
	status, bad := plane.call(t, http.MethodGet,
		base+"/investigations?incidentId=not-a-uuid", nil)
	if status != http.StatusBadRequest {
		t.Errorf("a malformed incident id = %d, want 400: %s", status, bad)
	}
}
