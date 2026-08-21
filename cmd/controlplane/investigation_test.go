package main

import (
	"net/http"
	"strings"

	"github.com/google/uuid"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

// Investigations at the composition seam: a real database, a fake vendor where Slack
// would be, and a scripted exchange at the model boundary. What is asserted is what
// an operator relies on: the tool universe the model is offered, subject inference and
// clarification, honest failure, refusal without a provider, and tenancy.

// openEpisode creates an alertmanager integration and delivers one firing alert, so an
// open episode exists to investigate.
func (p *integrationPlane) openEpisode(t *testing.T, alertname, fingerprint string) string {
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

	return p.episodeByTitle(t, alertname)
}

// episodeByTitle resolves the open episode a delivery just created.
func (p *integrationPlane) episodeByTitle(t *testing.T, title string) string {
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
	for _, episode := range listed.Items {
		if episode.Title == title {
			return episode.ID
		}
	}
	t.Fatalf("no episode titled %s: %s", title, body)
	return ""
}

// awaitInvestigation polls until the investigation leaves running.
func (p *integrationPlane) awaitInvestigation(t *testing.T, id string) string {
	t.Helper()
	return p.awaitInvestigationWithin(t, id, 30*time.Second)
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
	plane, vendor := autonomousPlaneWith(t, investigator, nil)

	if status, body := plane.createSlack(t, "Payments Bot Slack",
		"xoxb-good-token-1234"); status != http.StatusCreated {
		t.Fatalf("creating the bot-token slack = %d: %s", status, body)
	}
	vendor.accept("xoxp-user-token-5678")
	if status, body := plane.createSlack(t, "Payments User Slack",
		"xoxp-user-token-5678"); status != http.StatusCreated {
		t.Fatalf("creating the user-token slack = %d: %s", status, body)
	}

	episode := plane.openEpisode(t, "DiskFull", "finger-grants")
	status, body := plane.call(t, http.MethodPost, plane.base(surfaceOrg)+"/investigations",
		map[string]any{"episodeId": episode})
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
	plane, _ := autonomousPlaneWith(t, investigator, nil)
	base := plane.base(surfaceOrg)

	plane.openEpisode(t, "DiskFull", "finger-question")

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
	}, nil)
	base := plane.base(surfaceOrg)

	plane.openEpisode(t, "DiskFull", "finger-amb-1")
	plane.openEpisode(t, "DiskAlmostFull", "finger-amb-2")

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
	if strings.Contains(listing, `"running"`) {
		t.Error("a clarification must open nothing")
	}
}

func TestAFailedReasonerFailsTheInvestigationHonestly(t *testing.T) {
	exchange := &scriptedExchangeMain{
		failure: investigation.ErrReasonerUnavailable,
	}
	plane, _ := autonomousPlaneWith(t,
		&scriptedInvestigatorMain{exchange: exchange}, nil)
	base := plane.base(surfaceOrg)

	episode := plane.openEpisode(t, "DiskFull", "finger-fail")
	status, body := plane.call(t, http.MethodPost, base+"/investigations",
		map[string]any{"episodeId": episode})
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
	plane, _ := autonomousPlaneWith(t, nil, nil)
	base := plane.base(surfaceOrg)

	episode := plane.openEpisode(t, "DiskFull", "finger-noprov")
	status, body := plane.call(t, http.MethodPost, base+"/investigations",
		map[string]any{"episodeId": episode})
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
	}, func(cfg *config.Config) {
		cfg.Assignments[neighbourOrg] = "shared"
	})

	episode := plane.openEpisode(t, "DiskFull", "finger-tenant")
	status, body := plane.call(t, http.MethodPost,
		plane.base(surfaceOrg)+"/investigations", map[string]any{"episodeId": episode})
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

// openInvestigation opens one from an episode and reports its id.
func (p *integrationPlane) openInvestigation(t *testing.T, base, episode string) string {
	t.Helper()

	status, body := p.call(t, http.MethodPost, base+"/investigations",
		map[string]any{"episodeId": episode})
	if status != http.StatusAccepted {
		t.Fatalf("opening an investigation for %s = %d: %s", episode, status, body)
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
// wanting the investigations for one episode had to read every investigation the tenant
// has and narrow them in a browser. That is the shape the frontend spec forbids, and it
// is forbidden for a reason: a filter applied after paging disagrees with itself on page
// two.
func TestInvestigationsCanBeListedByTheEpisodeTheyCameFrom(t *testing.T) {
	investigator := &scriptedInvestigatorMain{exchange: &scriptedExchangeMain{}}
	plane, _ := autonomousPlaneWith(t, investigator, nil)
	base := plane.base(surfaceOrg)

	wanted := plane.openEpisode(t, "DiskFull", "finger-by-episode-1")
	other := plane.openEpisode(t, "HighLatency", "finger-by-episode-2")

	mine := plane.openInvestigation(t, base, wanted)
	theirs := plane.openInvestigation(t, base, other)
	plane.awaitInvestigation(t, mine)
	plane.awaitInvestigation(t, theirs)

	status, listing := plane.call(t, http.MethodGet,
		base+"/investigations?episodeId="+wanted, nil)
	if status != http.StatusOK {
		t.Fatalf("filtered listing = %d: %s", status, listing)
	}
	if !strings.Contains(listing, mine) {
		t.Errorf("the episode's own investigation is missing from its listing: %s", listing)
	}
	if strings.Contains(listing, theirs) {
		t.Errorf("another episode's investigation is in the listing: %s", listing)
	}

	// An episode this tenant does not have is an empty page, not an error and not a
	// disclosure: answering differently would let a caller probe for episode ids.
	status, empty := plane.call(t, http.MethodGet,
		base+"/investigations?episodeId="+uuid.NewString(), nil)
	if status != http.StatusOK {
		t.Fatalf("an unknown episode = %d, want an empty page: %s", status, empty)
	}
	if strings.Contains(empty, mine) || strings.Contains(empty, theirs) {
		t.Errorf("an unknown episode returned rows: %s", empty)
	}

	// A filter this listing does not serve is REFUSED rather than ignored. An ignored
	// filter returns everything while looking narrowed, which is the worse of the two.
	status, refused := plane.call(t, http.MethodGet,
		base+"/investigations?episode="+wanted, nil)
	if status != http.StatusBadRequest {
		t.Errorf("an unknown filter = %d, want 400: %s", status, refused)
	}

	// A value that is not an identifier is refused too, rather than reaching the database.
	status, bad := plane.call(t, http.MethodGet,
		base+"/investigations?episodeId=not-a-uuid", nil)
	if status != http.StatusBadRequest {
		t.Errorf("a malformed episode id = %d, want 400: %s", status, bad)
	}
}
