package controlplane

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/app"
	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

// Running one evaluation case: a whole control plane against the case's scripted world,
// the triggering alert delivered through real intake, the investigation opened through
// the real operator surface, and the ending read back with its provenance.

// evalRecord is what one case's run left behind — the raw material scoring reads.
type evalRecord struct {
	Case      string        `json:"case"`
	Attempt   int           `json:"attempt,omitempty"`
	Status    string        `json:"status"`
	Error     string        `json:"error,omitempty"`
	WallClock time.Duration `json:"wallClockNS"`
	Findings  []evalFinding `json:"findings"`
	Runs      []evalRun     `json:"runs"`
	Sources   []evalSource  `json:"sources"`
	Spend     evalSpend     `json:"spend"`
	// DistractorIntegrations are the integration ids seeded as irrelevant, so scoring
	// can count reads against them.
	DistractorIntegrations []string `json:"distractorIntegrations,omitempty"`

	// Answer is the direct reply the last turn gave. Empty for an alert-triggered
	// investigation, which was asked nothing and owes no prose.
	Answer            string             `json:"answer,omitempty"`
	ConclusionStatus  string             `json:"conclusionStatus,omitempty"`
	Actions           []evalAction       `json:"actions,omitempty"`
	Impact            evalImpact         `json:"impact,omitempty"`
	Hypotheses        []evalHypothesis   `json:"hypotheses,omitempty"`
	HypothesisUpdates [][]evalHypothesis `json:"hypothesisUpdates,omitempty"`
	Limitations       []evalLimitation   `json:"limitations,omitempty"`
	Postmortem        *evalPostmortem    `json:"postmortem,omitempty"`
	// Turns is the per-turn breakdown of a conversation. The fields above stay the union
	// across turns, so an incident case is scored exactly as it was before conversations
	// existed.
	Turns []evalTurn `json:"turns,omitempty"`
}

// evalTurn is one turn of a conversation: what was asked, what came back, and the reads
// that turn made.
type evalTurn struct {
	Turn             int           `json:"turn"`
	Question         string        `json:"question"`
	Answer           string        `json:"answer"`
	ConclusionStatus string        `json:"conclusionStatus,omitempty"`
	Status           string        `json:"status"`
	Findings         []evalFinding `json:"findings"`
	Runs             []evalRun     `json:"runs"`
}

type evalFinding struct {
	Statement  string `json:"statement"`
	Kind       string `json:"kind,omitempty"`
	Confidence string `json:"confidence,omitempty"`
	Mechanism  string `json:"mechanism,omitempty"`
	Sources    []int  `json:"runRefs"`
}

type evalAction struct {
	Title            string `json:"title"`
	Rationale        string `json:"rationale"`
	Verification     string `json:"verification"`
	RequiresApproval bool   `json:"requiresApproval"`
}

type evalImpact struct {
	CurrentState     string   `json:"currentState"`
	Summary          string   `json:"summary"`
	AffectedServices []string `json:"affectedServices"`
	AffectedUsers    []string `json:"affectedUsers"`
}

type evalHypothesis struct {
	ID        string `json:"id"`
	Statement string `json:"statement"`
	Status    string `json:"status"`
	Test      string `json:"test"`
	RunRefs   []int  `json:"runRefs"`
}

type evalPostmortem struct {
	Status      string                 `json:"status"`
	Impact      string                 `json:"impact"`
	Resolution  string                 `json:"resolution"`
	ActionItems []evalPostmortemAction `json:"actionItems"`
}

type evalPostmortemAction struct {
	Title    string `json:"title"`
	Owner    string `json:"owner"`
	Deadline string `json:"deadline"`
}

type evalLimitation struct {
	Statement string `json:"statement"`
}

type evalRun struct {
	Ordinal       int            `json:"ordinal"`
	IntegrationID string         `json:"integrationId"`
	Tool          string         `json:"tool"`
	Arguments     map[string]any `json:"arguments"`
	Outcome       string         `json:"outcome"`
	Truncated     bool           `json:"truncated"`
	Summary       string         `json:"summary"`
	Error         string         `json:"error,omitempty"`
}

type evalSource struct {
	IntegrationID string `json:"integrationId"`
	Rank          int    `json:"rank"`
	Reason        string `json:"reason"`
}

type evalSpend struct {
	InputTokens  int64 `json:"inputTokens"`
	OutputTokens int64 `json:"outputTokens"`
	MicroCents   int64 `json:"microCents"`
}

// evalModel is the deployment a real-model evaluation runs against, read from the
// provider-specific OC_EVAL_* environment by the gated test; zero means the scripted conversation.
type evalModel struct {
	Provider string
	Name     string
	Key      string
	Effort   string
	BaseURL  string
}

// runEvalCase executes one case and returns its record. A case with a question is a
// CONVERSATION and is asked through the operator surface; every other case is an incident
// and arrives as an alert. Both run against the same world and are scored by the same
// scorer, because what an investigation is worth does not depend on what triggered it.
func runEvalCase(
	t *testing.T, one evalCase, model evalModel, investigator investigation.Investigator,
) evalRecord {
	t.Helper()

	world, distractors := startEvalWorld(t, one, model, investigator)
	if one.Question != "" {
		return runEvalConversation(t, world, one, distractors, investigator)
	}
	return runEvalIncident(t, world, one, distractors, investigator)
}

// startEvalWorld brings up one case's whole plane: its own database, its own vendor fakes,
// and the integrations it is meant to read - plus the distractors it is meant not to.
func startEvalWorld(
	t *testing.T, one evalCase, model evalModel, investigator investigation.Investigator,
) (*integrationPlane, []string) {
	t.Helper()

	slackFake := newEvalSlackFake(t, one.Workspaces)
	slackFake.moreHistory = one.MoreHistory
	githubFake := newEvalGitHubFake(t, one.Installations)
	githubFake.failCommits = one.FailCommits
	githubFake.moreCommits = one.MoreCommits

	operatorAddress := freeAddress(t)
	intakeAddress := operatorAddress
	relayAddress := ""
	if len(one.Kubernetes.Workloads) > 0 {
		relayAddress = freeAddress(t)
	}
	var dsn string
	options := app.Options{
		Investigator: investigator, SlackAPIURL: slackFake.URL, GitHubAPIURL: githubFake.URL,
		ModelEffort: model.Effort, ModelBaseURL: model.BaseURL,
	}
	plane := startControlPlaneRunning(t, func(cfg *config.Config) {
		cfg.HTTPAddress = operatorAddress
		cfg.HTTPAddress = intakeAddress
		if relayAddress != "" {
			cfg.RelayAddress = relayAddress
			cfg.RelaySPKIPins = []string{base64.StdEncoding.EncodeToString(make([]byte, sha256.Size))}
		}
		dsn = cfg.DatabaseDSN
		digest := sha256.Sum256([]byte(surfaceToken))
		cfg.OperatorTokenDigest = digest[:]
		cfg.OperatorTokenOrganization = surfaceOrg
		// The production default window lead: the cases' fixtures sit at first-seen−40m
		// and −45m, which the derived window (first-seen − lead → now) must cover the
		// way a real deployment's would — the harness's zero-value config would
		// otherwise leave every scripted message and commit outside the window.
		cfg.GitHubAppID = "12345"
		cfg.GitHubAppKey = appKeyPEM(t)
		// A question needs the surface it is asked through, and a long conversation
		// needs a window small enough to overflow on a modest transcript rather than a
		// bought one.
		if model.Provider != "" {
			cfg.ModelProvider = model.Provider
			cfg.ModelName = model.Name
			cfg.ModelKey = model.Key
		}
	}, options)
	world := &integrationPlane{
		controlPlane: plane, operator: operatorAddress, intake: intakeAddress,
		relayAt: relayAddress, dsn: dsn,
	}
	if relayAddress != "" {
		startEvalKubernetes(t, world, one)
	}

	primarySlack := ""
	if len(one.Workspaces) > 0 {
		status, body := world.createSlack(t, "Payments Team Slack", evalPrimaryToken)
		if status != http.StatusCreated {
			t.Fatalf("creating the slack integration = %d: %s", status, body)
		}
		primarySlack = createdIntegrationID(t, status, body)
	}
	if len(one.Installations) > 0 {
		if status, body := world.createGitHub(t, "Payments GitHub",
			mustAtoi(t, evalPrimaryInstallation)); status != http.StatusCreated {
			t.Fatalf("creating the github integration = %d: %s", status, body)
		}
	}

	var distractors []string
	if one.DistractorSlackToken != "" {
		status, body := world.createSlack(t, "Marketing Slack", one.DistractorSlackToken)
		distractors = append(distractors, createdIntegrationID(t, status, body))
	}
	if one.DistractorInstallation != "" {
		status, body := world.createGitHub(t, "Website GitHub",
			mustAtoi(t, one.DistractorInstallation))
		distractors = append(distractors, createdIntegrationID(t, status, body))
	}
	if one.DistractorSlackToken != "" {
		bindEvaluationSlackCalls(investigator, primarySlack, distractors[0])
	}

	return world, distractors
}

func bindEvaluationSlackCalls(investigator investigation.Investigator, primary, distractor string) {
	scripted, ok := investigator.(*scriptedInvestigatorMain)
	if !ok || scripted.exchange == nil {
		return
	}
	scripted.exchange.mu.Lock()
	defer scripted.exchange.mu.Unlock()
	for move := range scripted.exchange.moves {
		for call := range scripted.exchange.moves[move].Calls {
			read := &scripted.exchange.moves[move].Calls[call]
			if !strings.HasPrefix(read.Tool, "slack.") || strings.Contains(read.Tool, "__") {
				continue
			}
			target := primary
			if read.Tool == "slack.list_channels" {
				target = distractor
			}
			read.Tool += "__" + strings.ReplaceAll(target, "-", "")
		}
	}
}

// runEvalIncident delivers the case's alert and reads the one investigation it opens.
func runEvalIncident(
	t *testing.T, world *integrationPlane, one evalCase, distractors []string,
	investigator investigation.Investigator,
) evalRecord {
	t.Helper()

	started := time.Now()
	incident := world.evalOpenIncident(t, one.Alertname, one.Labels)
	investigationID := world.awaitEvalInvestigation(t, incident.ID, evalCaseTimeout(investigator))
	final := world.awaitInvestigationWithin(t, investigationID, evalCaseTimeout(investigator))
	elapsed := time.Since(started)

	var read evalTurnRead
	decodeInto(t, final, &read)
	updates := world.evalHypothesisUpdates(t, investigationID)
	var draft *evalPostmortem
	if one.GeneratePostmortem {
		world.evalResolveIncident(t, incident)
		status, body := world.call(t, http.MethodPost,
			world.base(surfaceOrg)+"/incidents/"+incident.ID+"/postmortem", nil)
		if status != http.StatusCreated {
			t.Fatalf("generating the evaluation postmortem = %d: %s", status, body)
		}
		draft = &evalPostmortem{}
		decodeInto(t, body, draft)
	}

	return evalRecord{
		Case:                   one.Name,
		Status:                 read.Status,
		ConclusionStatus:       read.ConclusionStatus,
		Answer:                 read.Summary,
		Actions:                read.Actions,
		Impact:                 read.Impact,
		Hypotheses:             read.Hypotheses,
		HypothesisUpdates:      updates,
		Limitations:            read.Limitations,
		Postmortem:             draft,
		Error:                  read.Error,
		WallClock:              elapsed,
		Findings:               read.Findings,
		Runs:                   read.Runs,
		Sources:                read.Sources,
		Spend:                  read.Spend,
		DistractorIntegrations: distractors,
	}
}

func (p *integrationPlane) awaitEvalInvestigation(
	t *testing.T, incidentID string, within time.Duration,
) string {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		status, body := p.call(t, http.MethodGet,
			p.base(surfaceOrg)+"/investigations?incidentId="+incidentID, nil)
		if status != http.StatusOK {
			t.Fatalf("listing the incident's investigation = %d: %s", status, body)
		}
		var listed struct {
			Items []struct {
				ID string `json:"id"`
			} `json:"items"`
		}
		decodeInto(t, body, &listed)
		if len(listed.Items) > 0 {
			return listed.Items[0].ID
		}
		if time.Now().After(deadline) {
			t.Fatalf("the alert never opened an investigation: %s", body)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// runEvalConversation asks the case's question, then each follow-up, one turn at a time.
// The aggregate fields are the union across turns, so the scorer that grades an incident
// grades this without knowing which it is looking at.
func runEvalConversation(
	t *testing.T, world *integrationPlane, one evalCase, distractors []string,
	investigator investigation.Investigator,
) evalRecord {
	t.Helper()

	record := evalRecord{Case: one.Name, DistractorIntegrations: distractors}
	started := time.Now()

	conversation, turn := world.openConversation(t, one.Question, one.Question)
	if turn == "" {
		t.Fatal("opening a conversation with a question opened no turn")
	}

	for position, question := range append([]string{one.Question}, one.FollowUps...) {
		if position > 0 {
			opened, queued := world.say(t, conversation, question)
			if queued || opened == "" {
				t.Fatalf("turn %d was queued behind a run that had already ended",
					position+1)
			}
			turn = opened
		}
		body := world.awaitInvestigationWithin(t, turn, evalCaseTimeout(investigator))

		var read evalTurnRead
		decodeInto(t, body, &read)
		record.Turns = append(record.Turns, evalTurn{
			Turn: position + 1, Question: question, Answer: read.Summary,
			ConclusionStatus: read.ConclusionStatus, Status: read.Status,
			Findings: read.Findings, Runs: read.Runs,
		})

		// The aggregate is the union. Run ordinals are per-investigation, so a finding
		// and the runs it cites stay together: a cause is only ever scored against the
		// turn that made it.
		record.Status = read.Status
		record.Error = read.Error
		record.Answer = read.Summary
		record.ConclusionStatus = read.ConclusionStatus
		record.Actions = append(record.Actions, read.Actions...)
		record.Impact = read.Impact
		record.Hypotheses = append(record.Hypotheses, read.Hypotheses...)
		record.Limitations = append(record.Limitations, read.Limitations...)
		record.Findings = append(record.Findings, read.Findings...)
		record.Runs = append(record.Runs, read.Runs...)
		record.Sources = append(record.Sources, read.Sources...)
		record.Spend.InputTokens += read.Spend.InputTokens
		record.Spend.OutputTokens += read.Spend.OutputTokens
		record.Spend.MicroCents += read.Spend.MicroCents
	}
	record.WallClock = time.Since(started)
	return record
}

// evalTurnRead is one ended investigation as the operator surface returns it.
type evalTurnRead struct {
	Status           string           `json:"status"`
	ConclusionStatus string           `json:"conclusionStatus"`
	Error            string           `json:"error"`
	Summary          string           `json:"summary"`
	Findings         []evalFinding    `json:"findings"`
	Actions          []evalAction     `json:"actions"`
	Impact           evalImpact       `json:"impact"`
	Hypotheses       []evalHypothesis `json:"hypotheses"`
	Limitations      []evalLimitation `json:"limitations"`
	Spend            evalSpend        `json:"spend"`
	Sources          []evalSource     `json:"sources"`
	Runs             []evalRun        `json:"runs"`
}

// evalCaseTimeout sizes the wait for the model at the boundary: a scripted conversation
// answers instantly, a real one thinks for minutes across several turns.
func evalCaseTimeout(investigator investigation.Investigator) time.Duration {
	if investigator != nil {
		return 30 * time.Second
	}
	return 25 * time.Minute
}

// evalOpenIncident delivers one firing alert with the case's own labels and returns the
// incident it grouped into.
type evalIncident struct {
	ID            string
	IntegrationID string
	Secret        string
	Alertname     string
	Labels        map[string]string
	Started       time.Time
}

func (p *integrationPlane) evalOpenIncident(
	t *testing.T, alertname string, labels map[string]string,
) evalIncident {
	t.Helper()

	created := p.createAlertmanager(t, "Alertmanager for "+alertname)
	started := time.Now().UTC().Add(-15 * time.Minute)
	rendered := map[string]any{
		"groupKey": "group-" + alertname,
		"alerts": []map[string]any{{
			"status":      "firing",
			"fingerprint": "finger-" + alertname,
			"labels":      withAlertname(labels, alertname),
			"annotations": map[string]any{"summary": alertname + " is firing"},
			"startsAt":    started.Format(time.RFC3339),
		}},
	}
	if status, body := p.deliver(t, created.Integration.ID, created.WebhookSecret,
		mustJSON(t, rendered)); status != http.StatusAccepted {
		t.Fatalf("the seeding delivery = %d: %s", status, body)
	}
	return evalIncident{
		ID: p.incidentByTitle(t, evalIncidentTitle(alertname)), IntegrationID: created.Integration.ID,
		Secret: created.WebhookSecret, Alertname: alertname,
		Labels: withAlertname(labels, alertname), Started: started,
	}
}

func evalIncidentTitle(alertname string) string {
	if alertname == "" {
		return "unnamed alert"
	}
	return alertname
}

func (p *integrationPlane) evalResolveIncident(t *testing.T, incident evalIncident) {
	t.Helper()
	resolved := map[string]any{
		"groupKey": "group-" + incident.Alertname,
		"alerts": []map[string]any{{
			"status": "resolved", "fingerprint": "finger-" + incident.Alertname,
			"labels": incident.Labels, "annotations": map[string]any{"summary": incident.Alertname + " recovered"},
			"startsAt": incident.Started.Format(time.RFC3339),
			"endsAt":   time.Now().UTC().Format(time.RFC3339),
		}},
	}
	if status, body := p.deliver(t, incident.IntegrationID, incident.Secret,
		mustJSON(t, resolved)); status != http.StatusAccepted {
		t.Fatalf("the resolution delivery = %d: %s", status, body)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		status, body := p.call(t, http.MethodGet,
			p.base(surfaceOrg)+"/incidents/"+incident.ID, nil)
		if status != http.StatusOK {
			t.Fatalf("reading the resolving incident = %d: %s", status, body)
		}
		var read struct {
			Status string `json:"status"`
		}
		decodeInto(t, body, &read)
		if read.Status == "resolved" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the incident never resolved: %s", body)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (p *integrationPlane) evalHypothesisUpdates(t *testing.T, investigationID string) [][]evalHypothesis {
	t.Helper()
	status, body := p.call(t, http.MethodGet,
		p.base(surfaceOrg)+"/investigations/"+investigationID+"/activity", nil)
	if status != http.StatusOK {
		t.Fatalf("reading evaluation activity = %d: %s", status, body)
	}
	var activity struct {
		Items []struct {
			Type    string `json:"type"`
			Payload struct {
				Hypotheses []evalHypothesis `json:"hypotheses"`
			} `json:"payload"`
		} `json:"items"`
	}
	decodeInto(t, body, &activity)
	var updates [][]evalHypothesis
	for _, item := range activity.Items {
		if item.Type == "hypotheses_updated" {
			updates = append(updates, item.Payload.Hypotheses)
		}
	}
	return updates
}

// awaitInvestigationWithin polls until the investigation leaves its active states, with a
// case-supplied deadline instead of the feature tests' fixed one.
func (p *integrationPlane) awaitInvestigationWithin(
	t *testing.T, id string, within time.Duration,
) string {
	t.Helper()

	deadline := time.Now().Add(within)
	for {
		status, body := p.call(t, http.MethodGet,
			p.base(surfaceOrg)+"/investigations/"+id, nil)
		if status != http.StatusOK {
			t.Fatalf("reading the investigation = %d: %s", status, body)
		}
		var read struct {
			Status string `json:"status"`
		}
		decodeInto(t, body, &read)
		if !investigationActive(read.Status) {
			return body
		}
		if time.Now().After(deadline) {
			t.Fatalf("the investigation never ended: %s", body)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func withAlertname(labels map[string]string, alertname string) map[string]string {
	named := map[string]string{"alertname": alertname}
	for key, value := range labels {
		named[key] = value
	}
	return named
}

func createdIntegrationID(t *testing.T, status int, body string) string {
	t.Helper()
	if status != http.StatusCreated {
		t.Fatalf("creating a distractor integration = %d: %s", status, body)
	}
	var created createdBody
	decodeInto(t, body, &created)
	return created.Integration.ID
}
