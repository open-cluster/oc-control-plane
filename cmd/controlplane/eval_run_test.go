package main

import (
	"crypto/sha256"
	"net/http"
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
	Answer string `json:"answer,omitempty"`
	// Turns is the per-turn breakdown of a conversation. The fields above stay the union
	// across turns, so an incident case is scored exactly as it was before conversations
	// existed.
	Turns []evalTurn `json:"turns,omitempty"`
	// Compactions is how many times the conversation was compacted, counted from the
	// turns' own event streams rather than from anything the model said.
	Compactions int `json:"compactions,omitempty"`
}

// evalTurn is one turn of a conversation: what was asked, what came back, and the reads
// that turn made.
type evalTurn struct {
	Turn     int           `json:"turn"`
	Question string        `json:"question"`
	Answer   string        `json:"answer"`
	Status   string        `json:"status"`
	Findings []evalFinding `json:"findings"`
	Runs     []evalRun     `json:"runs"`
}

type evalFinding struct {
	Statement string `json:"statement"`
	Kind      string `json:"kind,omitempty"`
	Sources   []int  `json:"sources"`
}

type evalRun struct {
	Ordinal       int            `json:"ordinal"`
	IntegrationID string         `json:"integrationId"`
	Tool          string         `json:"tool"`
	Arguments     map[string]any `json:"arguments"`
	Outcome       string         `json:"outcome"`
	Truncated     bool           `json:"truncated"`
	Summary       string         `json:"summary"`
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
// OC_EVAL_MODEL_* environment by the gated test; zero means the scripted conversation.
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
	plane := startControlPlaneRunning(t, func(cfg *config.Config) {
		cfg.OperatorAddress = operatorAddress
		cfg.IntakeAddress = intakeAddress
		digest := sha256.Sum256([]byte(surfaceToken))
		cfg.OperatorTokenDigest = digest[:]
		cfg.OperatorTokenOrganization = surfaceOrg
		// The production default window lead: the cases' fixtures sit at first-seen−40m
		// and −45m, which the derived window (first-seen − lead → now) must cover the
		// way a real deployment's would — the harness's zero-value config would
		// otherwise leave every scripted message and commit outside the window.
		cfg.InvestigationWindowLead = 2 * time.Hour
		cfg.SlackAPIURL = slackFake.URL
		cfg.GitHubAPIURL = githubFake.URL
		cfg.GitHubAppID = "12345"
		cfg.GitHubAppKey = appKeyPEM(t)
		// A question needs the surface it is asked through, and a long conversation
		// needs a window small enough to overflow on a modest transcript rather than a
		// bought one.
		cfg.ConversationsEnabled = one.Question != ""
		if one.ContextWindow > 0 {
			cfg.ModelContextWindow = one.ContextWindow
		}
		if one.ContextThresholdPercent > 0 {
			cfg.ContextThresholdPercent = one.ContextThresholdPercent
		}
		if model.Provider != "" {
			cfg.ModelProvider = model.Provider
			cfg.ModelName = model.Name
			cfg.ModelKey = model.Key
			cfg.ModelEffort = model.Effort
			cfg.ModelBaseURL = model.BaseURL
			cfg.ModelConsented = []string{model.Provider}
		}
	}, app.Options{Investigator: investigator})
	world := &integrationPlane{
		controlPlane: plane, operator: operatorAddress, intake: intakeAddress,
	}

	if len(one.Workspaces) > 0 {
		if status, body := world.createSlack(t, "Payments Team Slack",
			evalPrimaryToken); status != http.StatusCreated {
			t.Fatalf("creating the slack integration = %d: %s", status, body)
		}
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

	return world, distractors
}

// runEvalIncident delivers the case's alert and reads the one investigation it opens.
func runEvalIncident(
	t *testing.T, world *integrationPlane, one evalCase, distractors []string,
	investigator investigation.Investigator,
) evalRecord {
	t.Helper()

	episode := world.evalOpenEpisode(t, one.Alertname, one.Labels)
	status, body := world.call(t, http.MethodPost, world.base(surfaceOrg)+"/investigations",
		map[string]any{"episodeId": episode})
	if status != http.StatusAccepted {
		t.Fatalf("opening the investigation = %d: %s", status, body)
	}
	var opened struct {
		ID string `json:"id"`
	}
	decodeInto(t, body, &opened)

	started := time.Now()
	final := world.awaitInvestigationWithin(t, opened.ID, evalCaseTimeout(investigator))
	elapsed := time.Since(started)

	var read evalTurnRead
	decodeInto(t, final, &read)

	return evalRecord{
		Case:                   one.Name,
		Status:                 read.Status,
		Error:                  read.Error,
		WallClock:              elapsed,
		Findings:               read.Findings,
		Runs:                   read.Runs,
		Sources:                read.Sources,
		Spend:                  read.Spend,
		DistractorIntegrations: distractors,
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
			Turn: position + 1, Question: question, Answer: read.Answer,
			Status: read.Status, Findings: read.Findings, Runs: read.Runs,
		})

		// The aggregate is the union. Run ordinals are per-investigation, so a finding
		// and the runs it cites stay together: a cause is only ever scored against the
		// turn that made it.
		record.Status = read.Status
		record.Error = read.Error
		record.Answer = read.Answer
		record.Findings = append(record.Findings, read.Findings...)
		record.Runs = append(record.Runs, read.Runs...)
		record.Sources = append(record.Sources, read.Sources...)
		record.Spend.InputTokens += read.Spend.InputTokens
		record.Spend.OutputTokens += read.Spend.OutputTokens
		record.Spend.MicroCents += read.Spend.MicroCents
		record.Compactions += countCompactions(t, world, turn)
	}
	record.WallClock = time.Since(started)
	return record
}

// evalTurnRead is one ended investigation as the operator surface returns it.
type evalTurnRead struct {
	Status   string        `json:"status"`
	Error    string        `json:"error"`
	Answer   string        `json:"answer"`
	Findings []evalFinding `json:"findings"`
	Spend    evalSpend     `json:"spend"`
	Sources  []evalSource  `json:"sources"`
	Runs     []evalRun     `json:"runs"`
}

// countCompactions reads one turn's event stream and counts what it says about memory.
// It is counted from the STREAM rather than from anything the model said, because the
// whole point of the measure is that it does not rely on the model being honest about it.
func countCompactions(t *testing.T, world *integrationPlane, id string) int {
	t.Helper()

	status, body := world.call(t, http.MethodGet,
		world.base(surfaceOrg)+"/investigations/"+id+"/events", nil)
	if status != http.StatusOK {
		t.Fatalf("reading the event stream = %d: %s", status, body)
	}
	compactions := 0
	for _, event := range parseEventStream(t, body) {
		if event.kind == "compacted" {
			compactions++
		}
	}
	return compactions
}

// evalCaseTimeout sizes the wait for the model at the boundary: a scripted conversation
// answers instantly, a real one thinks for minutes across several turns.
func evalCaseTimeout(investigator investigation.Investigator) time.Duration {
	if investigator != nil {
		return 30 * time.Second
	}
	return 25 * time.Minute
}

// evalOpenEpisode delivers one firing alert with the case's own labels and returns the
// episode it grouped into.
func (p *integrationPlane) evalOpenEpisode(
	t *testing.T, alertname string, labels map[string]string,
) string {
	t.Helper()

	created := p.createAlertmanager(t, "Alertmanager for "+alertname)
	rendered := map[string]any{
		"groupKey": "group-" + alertname,
		"alerts": []map[string]any{{
			"status":      "firing",
			"fingerprint": "finger-" + alertname,
			"labels":      withAlertname(labels, alertname),
			"annotations": map[string]any{"summary": alertname + " is firing"},
			"startsAt":    time.Now().UTC().Add(-15 * time.Minute).Format(time.RFC3339),
		}},
	}
	if status, body := p.deliver(t, created.Integration.ID, created.WebhookSecret,
		mustJSON(t, rendered)); status != http.StatusAccepted {
		t.Fatalf("the seeding delivery = %d: %s", status, body)
	}
	return p.episodeByTitle(t, alertname)
}

// awaitInvestigationWithin polls until the investigation leaves running, with a
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
		if read.Status != "running" {
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
