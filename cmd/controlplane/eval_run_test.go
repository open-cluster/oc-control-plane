package main

import (
	"crypto/sha256"
	"net/http"
	"testing"
	"time"

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
}

type evalFinding struct {
	Statement string `json:"statement"`
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
// OC_EVAL_MODEL_* environment by the gated test; zero means the scripted reasoner.
// Architecture selects which loop investigates — issue #5's candidates are captured by
// running the same worlds under each.
type evalModel struct {
	Provider     string
	Name         string
	Key          string
	Effort       string
	BaseURL      string
	Architecture string
}

// runEvalCase executes one case and returns its record. Each case gets its own plane and
// database; the fakes are the case's own, so worlds cannot bleed into each other.
func runEvalCase(
	t *testing.T, one evalCase, model evalModel, reasoner investigation.Reasoner,
) evalRecord {
	t.Helper()

	slackFake := newEvalSlackFake(t, one.Workspaces)
	slackFake.moreHistory = one.MoreHistory
	githubFake := newEvalGitHubFake(t, one.Installations)
	githubFake.failCommits = one.FailCommits
	githubFake.moreCommits = one.MoreCommits

	operatorAddress := freeAddress(t)
	intakeAddress := freeAddress(t)
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
		if model.Provider != "" {
			cfg.ModelProvider = model.Provider
			cfg.ModelName = model.Name
			cfg.ModelKey = model.Key
			cfg.ModelEffort = model.Effort
			cfg.ModelBaseURL = model.BaseURL
			cfg.ModelConsented = []string{model.Provider}
		}
		if model.Architecture != "" {
			cfg.InvestigationArchitecture = model.Architecture
		}
	}, wiring{reasoner: reasoner})
	world := &integrationPlane{
		controlPlane: plane, operator: operatorAddress, intake: intakeAddress,
	}

	if status, body := world.createSlack(t, "Payments Team Slack",
		evalPrimaryToken); status != http.StatusCreated {
		t.Fatalf("creating the slack integration = %d: %s", status, body)
	}
	if status, body := world.createGitHub(t, "Payments GitHub",
		mustAtoi(t, evalPrimaryInstallation)); status != http.StatusCreated {
		t.Fatalf("creating the github integration = %d: %s", status, body)
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
	final := world.awaitInvestigationWithin(t, opened.ID, evalCaseTimeout(reasoner))
	elapsed := time.Since(started)

	var read struct {
		Status   string        `json:"status"`
		Error    string        `json:"error"`
		Findings []evalFinding `json:"findings"`
		Spend    evalSpend     `json:"spend"`
		Sources  []evalSource  `json:"sources"`
		Runs     []evalRun     `json:"runs"`
	}
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

// evalCaseTimeout sizes the wait for the model at the boundary: a scripted reasoner
// answers instantly, a real one thinks for minutes across several rounds.
func evalCaseTimeout(reasoner investigation.Reasoner) time.Duration {
	if reasoner != nil {
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
