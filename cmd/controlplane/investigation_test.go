package main

import (
	"context"
	"crypto/sha256"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

// Investigations at the composition seam: a real database, a fake vendor where Slack
// would be, and a scripted reasoner standing at the model boundary — the surviving test
// seam of the recorded-transcript mechanism. What is asserted is the provenance an
// operator audits: which sources were selected and why, which tools ran with what scope,
// what each returned, and findings that cite the runs that support them.

// scriptedReasoner plays back a fixed sequence of decisions and keeps every brief it was
// shown, so a test can assert what the reasoner actually saw.
type scriptedReasoner struct {
	mu        sync.Mutex
	decisions []investigation.Decision
	failure   error
	briefs    []investigation.Brief
}

func (s *scriptedReasoner) Decide(
	_ context.Context, brief investigation.Brief,
) (investigation.Decision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.briefs = append(s.briefs, brief)
	if s.failure != nil {
		return investigation.Decision{}, s.failure
	}
	if len(s.decisions) == 0 {
		return investigation.Decision{}, nil
	}
	next := s.decisions[0]
	s.decisions = s.decisions[1:]
	return next, nil
}

func (s *scriptedReasoner) seen() []investigation.Brief {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]investigation.Brief(nil), s.briefs...)
}

// investigationPlaneWith starts a plane with intake, operator surface, a Slack fake and
// the scripted reasoner at the model boundary.
func investigationPlaneWith(
	t *testing.T, reasoner investigation.Reasoner,
) (*integrationPlane, *vendorFake) {
	t.Helper()

	vendor := newVendorFake(t, "xoxb-good-token-1234")
	operatorAddress := freeAddress(t)
	intakeAddress := freeAddress(t)
	plane := startControlPlaneRunning(t, func(cfg *config.Config) {
		cfg.OperatorAddress = operatorAddress
		cfg.IntakeAddress = intakeAddress
		digest := sha256.Sum256([]byte(surfaceToken))
		cfg.OperatorTokenDigest = digest[:]
		cfg.OperatorTokenOrganization = surfaceOrg
		cfg.Assignments[neighbourOrg] = "shared"
		cfg.SlackAPIURL = vendor.URL
	}, wiring{reasoner: reasoner})
	return &integrationPlane{
		controlPlane: plane, operator: operatorAddress, intake: intakeAddress,
	}, vendor
}

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

func TestInvestigationRecordsItsWholeProvenance(t *testing.T) {
	reasoner := &scriptedReasoner{decisions: []investigation.Decision{
		{
			Calls: []investigation.ToolCall{{
				Tool:      "slack.list_channels",
				Arguments: map[string]any{"nameContains": "incident"},
			}},
			Spend: investigation.Spend{InputTokens: 100, OutputTokens: 20, MicroCents: 7},
		},
		{
			Findings: []investigation.Finding{{
				Statement: "the incident channel names a deploy at the window's start",
				Sources:   []int{1},
			}},
			Spend: investigation.Spend{InputTokens: 150, OutputTokens: 30, MicroCents: 11},
		},
	}}
	plane, vendor := investigationPlaneWith(t, reasoner)
	vendor.serveChannels(`{"ok":true,"channels":[
		{"id":"C1","name":"incidents","topic":{"value":"live incident chat"}}]}`)
	base := plane.base(surfaceOrg)

	// A Slack integration for the router to select.
	if status, body := plane.createSlack(t, "Acme Slack", "xoxb-good-token-1234"); status != http.StatusCreated {
		t.Fatalf("creating slack = %d: %s", status, body)
	}
	episode := plane.openEpisode(t, "DiskFull", "finger-provenance")

	status, body := plane.call(t, http.MethodPost, base+"/investigations",
		map[string]any{"episodeId": episode})
	if status != http.StatusAccepted {
		t.Fatalf("opening an investigation = %d: %s", status, body)
	}
	var opened struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	decodeInto(t, body, &opened)
	if opened.Status != "running" {
		t.Errorf("a fresh investigation is %q, want running", opened.Status)
	}

	final := plane.awaitInvestigation(t, opened.ID)
	var read struct {
		Status        string `json:"status"`
		Subject       string `json:"subject"`
		EpisodeID     string `json:"episodeId"`
		IntegrationID string `json:"integrationId"`
		Findings      []struct {
			Statement string `json:"statement"`
			Sources   []int  `json:"sources"`
		} `json:"findings"`
		Spend struct {
			InputTokens int64 `json:"inputTokens"`
			MicroCents  int64 `json:"microCents"`
		} `json:"spend"`
		Sources []struct {
			IntegrationID string `json:"integrationId"`
			Reason        string `json:"reason"`
		} `json:"sources"`
		Runs []struct {
			Ordinal   int            `json:"ordinal"`
			Tool      string         `json:"tool"`
			Outcome   string         `json:"outcome"`
			Arguments map[string]any `json:"arguments"`
		} `json:"runs"`
	}
	decodeInto(t, final, &read)

	if read.Status != "concluded" {
		t.Fatalf("status = %q, want concluded: %s", read.Status, final)
	}
	if read.Subject != "DiskFull" || read.EpisodeID != episode {
		t.Errorf("the trigger is not on the record: subject=%q episode=%q",
			read.Subject, read.EpisodeID)
	}
	if len(read.Sources) == 0 || read.Sources[0].Reason == "" {
		t.Errorf("no routed source with a reason: %s", final)
	}
	if len(read.Runs) != 1 || read.Runs[0].Tool != "slack.list_channels" ||
		read.Runs[0].Outcome != "succeeded" {
		t.Fatalf("runs = %+v", read.Runs)
	}
	if read.Runs[0].Arguments["nameContains"] != "incident" {
		t.Errorf("the run's scope is not on the record: %+v", read.Runs[0].Arguments)
	}
	if len(read.Findings) != 1 || len(read.Findings[0].Sources) == 0 ||
		read.Findings[0].Sources[0] != 1 {
		t.Errorf("findings do not cite their run: %+v", read.Findings)
	}
	if read.Spend.InputTokens != 250 || read.Spend.MicroCents != 18 {
		t.Errorf("spend = %+v, want the two decisions summed", read.Spend)
	}
	if strings.Contains(string(final), `"stoppedBy"`) {
		t.Errorf("a free conclusion must carry no stoppedBy label: %s", final)
	}

	t.Run("the reasoner saw the run's content, not a summary of one", func(t *testing.T) {
		briefs := reasoner.seen()
		if len(briefs) != 2 {
			t.Fatalf("the reasoner decided %d times, want 2", len(briefs))
		}
		second := briefs[1]
		if len(second.Runs) != 1 || second.Runs[0].Content == nil {
			t.Errorf("the second decision was made without the first run's content: %+v",
				second.Runs)
		}
	})
}

// A reasoner that never wants to stop is forced to conclude, and the wire view labels
// the outcome with what stopped it — resource exhaustion is never rendered as a free
// diagnosis.
func TestAForcedConclusionIsLabeledStoppedOnTheWire(t *testing.T) {
	restless := make([]investigation.Decision, 0, 6)
	for range 5 {
		restless = append(restless, investigation.Decision{
			Calls: []investigation.ToolCall{{Tool: "slack.list_channels"}},
		})
	}
	restless = append(restless, investigation.Decision{})
	reasoner := &scriptedReasoner{decisions: restless}
	plane, vendor := investigationPlaneWith(t, reasoner)
	vendor.serveChannels(`{"ok":true,"channels":[
		{"id":"C1","name":"incidents","topic":{"value":"live incident chat"}}]}`)
	base := plane.base(surfaceOrg)

	if status, body := plane.createSlack(t, "Acme Slack", "xoxb-good-token-1234"); status != http.StatusCreated {
		t.Fatalf("creating slack = %d: %s", status, body)
	}
	episode := plane.openEpisode(t, "DiskFull", "finger-stopped")

	status, body := plane.call(t, http.MethodPost, base+"/investigations",
		map[string]any{"episodeId": episode})
	if status != http.StatusAccepted {
		t.Fatalf("opening an investigation = %d: %s", status, body)
	}
	var opened struct {
		ID string `json:"id"`
	}
	decodeInto(t, body, &opened)

	final := plane.awaitInvestigation(t, opened.ID)
	var read struct {
		Status    string `json:"status"`
		StoppedBy string `json:"stoppedBy"`
	}
	decodeInto(t, final, &read)
	if read.Status != "concluded" {
		t.Fatalf("status = %q; a forced stop is a conclusion, not a failure: %s",
			read.Status, final)
	}
	if read.StoppedBy != "reasoner_turns" {
		t.Errorf("stoppedBy = %q, want reasoner_turns: %s", read.StoppedBy, final)
	}
}

// The org-scoped tool universe is filtered by each Integration's verified grants before
// the model sees it: a pasted bot token — whose recorded grants can never include
// user_token — is offered the three grant-supported Slack tools and NOT
// slack.search_messages, while a user token with the search scope is offered all four.
// Asserted at the model boundary itself: Brief.Sources is exactly what the reasoner may
// choose from, so a tool absent there is a capability the model was never offered.
func TestToolUniverseIsFilteredByVerifiedGrants(t *testing.T) {
	reasoner := &scriptedReasoner{}
	plane, vendor := investigationPlaneWith(t, reasoner)

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

	briefs := reasoner.seen()
	if len(briefs) == 0 {
		t.Fatal("the reasoner never decided")
	}
	offered := map[string][]string{}
	for _, source := range briefs[0].Sources {
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
	reasoner := &scriptedReasoner{}
	plane, _ := investigationPlaneWith(t, reasoner)
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
	plane, _ := investigationPlaneWith(t, &scriptedReasoner{})
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
	reasoner := &scriptedReasoner{failure: investigation.ErrReasonerUnavailable}
	plane, _ := investigationPlaneWith(t, reasoner)
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
	plane, _ := investigationPlaneWith(t, nil)
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
	reasoner := &scriptedReasoner{}
	plane, _ := investigationPlaneWith(t, reasoner)

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
