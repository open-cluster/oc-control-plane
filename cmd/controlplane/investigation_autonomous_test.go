package main

import (
	"context"
	"crypto/sha256"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

// The autonomous loop through the whole composed plane: a real database, real intake,
// the real operator surface, a real Slack fake at the vendor seam — and a scripted
// exchange at the model boundary. What is pinned is what an operator reads
// afterwards: the offer recorded as sources, every run in provenance, the §7 conclusion
// with kinds, confidence and next steps, and an honest stopped-by label when a ceiling
// forces the end.

// scriptedExchangeMain plays back moves and remembers what it was fed.
type scriptedExchangeMain struct {
	mu      sync.Mutex
	moves   []investigation.Move
	failure error
	fed     []bool // each Next's mustConclude
	// results is every fed-back call result, in order — what the model actually read.
	results []investigation.CallResult
}

func (s *scriptedExchangeMain) Next(
	_ context.Context, results []investigation.CallResult, mustConclude bool, _ string,
) (investigation.Move, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fed = append(s.fed, mustConclude)
	s.results = append(s.results, results...)
	if s.failure != nil {
		return investigation.Move{}, s.failure
	}
	if len(s.moves) == 0 {
		return investigation.Move{Conclusion: &investigation.Conclusion{}}, nil
	}
	next := s.moves[0]
	s.moves = s.moves[1:]
	return next, nil
}

type scriptedInvestigatorMain struct {
	mu          sync.Mutex
	exchange    *scriptedExchangeMain
	orientation investigation.Orientation
}

func (s *scriptedInvestigatorMain) OpenExchange(
	_ context.Context, orientation investigation.Orientation,
) (investigation.Exchange, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.orientation = orientation
	return s.exchange, nil
}

// autonomousPlaneWith starts a plane whose model boundary is the scripted
// exchange, with any further config the case needs.
func autonomousPlaneWith(
	t *testing.T, investigator investigation.Investigator,
	adjust func(cfg *config.Config),
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
		cfg.SlackAPIURL = vendor.URL
		if adjust != nil {
			adjust(cfg)
		}
	}, wiring{investigator: investigator})
	return &integrationPlane{
		controlPlane: plane, operator: operatorAddress, intake: intakeAddress,
	}, vendor
}

func TestAutonomousInvestigationRecordsTheStructuredConclusion(t *testing.T) {
	t.Parallel()

	exchange := &scriptedExchangeMain{moves: []investigation.Move{
		{Calls: []investigation.AgentCall{{
			ID: "call-1", Tool: "slack.list_channels", Arguments: map[string]any{},
		}}, Spend: investigation.Spend{InputTokens: 100, OutputTokens: 10, MicroCents: 5}},
		{Conclusion: &investigation.Conclusion{
			Findings: []investigation.Finding{
				{Statement: "the incident channel shows the deploy went out",
					Kind:       investigation.FindingTriggeringChange,
					Confidence: investigation.ConfidenceLikely,
					Sources:    []int{1}},
				{Statement: "no dns change was involved",
					Kind:       investigation.FindingRuledOut,
					Confidence: investigation.ConfidenceConfirmed,
					Sources:    []int{1}},
			},
			NextSteps: []string{"roll back the deploy", "watch the latency panel"},
		}, Spend: investigation.Spend{InputTokens: 120, OutputTokens: 30, MicroCents: 7}},
	}}
	investigator := &scriptedInvestigatorMain{exchange: exchange}
	plane, vendor := autonomousPlaneWith(t, investigator, nil)
	vendor.serveChannels(`{"ok":true,"channels":[
		{"id":"C1","name":"incidents","topic":{"value":"live incident chat"}}],
		"response_metadata":{"next_cursor":""}}`)

	if status, body := plane.createSlack(t, "Payments Slack",
		"xoxb-good-token-1234"); status != http.StatusCreated {
		t.Fatalf("creating the slack integration = %d: %s", status, body)
	}
	episode := plane.openEpisode(t, "HighLatency", "finger-auto-1")
	status, body := plane.call(t, http.MethodPost, plane.base(surfaceOrg)+"/investigations",
		map[string]any{"episodeId": episode})
	if status != http.StatusAccepted {
		t.Fatalf("opening the investigation = %d: %s", status, body)
	}
	var opened struct {
		ID string `json:"id"`
	}
	decodeInto(t, body, &opened)
	final := plane.awaitInvestigation(t, opened.ID)

	var read struct {
		Status   string `json:"status"`
		Findings []struct {
			Statement  string `json:"statement"`
			Kind       string `json:"kind"`
			Confidence string `json:"confidence"`
			Sources    []int  `json:"sources"`
		} `json:"findings"`
		NextSteps []string `json:"nextSteps"`
		StoppedBy string   `json:"stoppedBy"`
		Spend     struct {
			MicroCents int64 `json:"microCents"`
		} `json:"spend"`
		Sources []struct {
			Reason string `json:"reason"`
		} `json:"sources"`
		Runs []struct {
			Ordinal int    `json:"ordinal"`
			Tool    string `json:"tool"`
			Outcome string `json:"outcome"`
		} `json:"runs"`
	}
	decodeInto(t, final, &read)

	if read.Status != "concluded" || read.StoppedBy != "" {
		t.Fatalf("status=%q stoppedBy=%q: %s", read.Status, read.StoppedBy, final)
	}
	if len(read.Findings) != 2 ||
		read.Findings[0].Kind != "triggering_change" ||
		read.Findings[0].Confidence != "likely" ||
		read.Findings[1].Kind != "ruled_out" {
		t.Errorf("findings = %+v; the §7 conclusion must survive to the operator", read.Findings)
	}
	if len(read.NextSteps) != 2 || read.NextSteps[0] != "roll back the deploy" {
		t.Errorf("next steps = %+v", read.NextSteps)
	}
	if read.Spend.MicroCents != 12 {
		t.Errorf("spend = %+v; every move summed", read.Spend)
	}
	if len(read.Sources) != 1 || !strings.Contains(read.Sources[0].Reason, "offered") {
		t.Errorf("sources = %+v; the offer is the recorded reason", read.Sources)
	}
	if len(read.Runs) != 1 || read.Runs[0].Tool != "slack.list_channels" ||
		read.Runs[0].Outcome != "succeeded" {
		t.Errorf("runs = %+v", read.Runs)
	}

	orientation := investigator.orientation
	if orientation.Subject == "" || len(orientation.Sources) != 1 {
		t.Errorf("orientation = %+v; the investigator is oriented from held context",
			orientation)
	}
	if orientation.Trigger == nil || orientation.Trigger.Labels["namespace"] != "payments" {
		t.Errorf("trigger = %+v; the alert's own labels are held context", orientation.Trigger)
	}

	// The exchange read the run's content, not a summary of one.
	if len(exchange.results) != 1 || exchange.results[0].Run.Content == nil {
		t.Errorf("the exchange was fed %+v; the next turn needs the run's content",
			exchange.results)
	}
}

func TestAutonomousTurnCeilingIsConfigurationAndStopsHonestly(t *testing.T) {
	t.Parallel()

	exchange := &scriptedExchangeMain{moves: []investigation.Move{
		{Calls: []investigation.AgentCall{{
			ID: "call-1", Tool: "slack.list_channels", Arguments: map[string]any{},
		}}},
		{Conclusion: &investigation.Conclusion{Findings: []investigation.Finding{{
			Statement:  "the reads that ran show a deploy announcement",
			Kind:       investigation.FindingUnresolvedLead,
			Confidence: investigation.ConfidencePossible,
			Sources:    []int{1},
		}}}},
	}}
	plane, vendor := autonomousPlaneWith(t, &scriptedInvestigatorMain{
		exchange: exchange,
	}, func(cfg *config.Config) {
		cfg.InvestigationMaxTurns = 1
	})
	vendor.serveChannels(`{"ok":true,"channels":[
		{"id":"C1","name":"incidents","topic":{"value":"live incident chat"}}],
		"response_metadata":{"next_cursor":""}}`)

	if status, body := plane.createSlack(t, "Payments Slack",
		"xoxb-good-token-1234"); status != http.StatusCreated {
		t.Fatalf("creating the slack integration = %d: %s", status, body)
	}
	episode := plane.openEpisode(t, "TurnCeiling", "finger-auto-2")
	status, body := plane.call(t, http.MethodPost, plane.base(surfaceOrg)+"/investigations",
		map[string]any{"episodeId": episode})
	if status != http.StatusAccepted {
		t.Fatalf("opening the investigation = %d: %s", status, body)
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
	if read.Status != "concluded" || read.StoppedBy != "reasoner_turns" {
		t.Errorf("status=%q stoppedBy=%q; a configured ceiling ends in an honest stop: %s",
			read.Status, read.StoppedBy, final)
	}
	if len(exchange.fed) != 2 || exchange.fed[0] || !exchange.fed[1] {
		t.Errorf("fed = %v; one reading turn, then the forced conclusion", exchange.fed)
	}
}
