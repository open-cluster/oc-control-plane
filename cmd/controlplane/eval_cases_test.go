package main

import (
	"net/http"
	"strings"
	"time"
)

// The evaluation cases: scripted incident worlds with ground truth, per issue #5 §11.
// Each archetype exists to expose one failure mode an investigator can have; the ground
// truth is what a correct investigation of that world establishes, spelled as markers a
// scorer can check without judgement.

// causeTruth is one real cause in a world. It counts as found when a finding's statement
// carries any marker AND cites a run of the named tool — a statement without provenance
// is not a found cause.
type causeTruth struct {
	Name    string
	Markers []string
	Tool    string
}

// readTruth is one read a correct investigation makes: the discriminating reads whose
// absence signals premature termination.
type readTruth struct {
	Tool string
	// ArgMarker must appear in the run's rendered arguments; empty matches any call of
	// the tool.
	ArgMarker string
}

// groundTruth is everything the scorer knows about a world.
type groundTruth struct {
	Causes []causeTruth
	// MustNotClaim are deception or unknowable markers: a finding carrying one is a
	// false claim.
	MustNotClaim []string
	// Discriminating are the reads separating competing explanations.
	Discriminating []readTruth
	// RelevantTools is the tool-selection baseline for precision and recall.
	RelevantTools []string
	// ExpectFindings false means the honest conclusion is no findings at all.
	ExpectFindings bool
}

// evalCase is one world, its trigger, and its truth.
type evalCase struct {
	Name      string
	Alertname string
	Labels    map[string]string
	// Slack workspaces by token; the primary integration pastes primaryToken.
	Workspaces map[string]evalWorkspace
	// Installations by id; the primary integration configures primaryInstallation.
	Installations map[string]evalInstallation
	// Distractors add extra integrations whose reads are irrelevant by construction.
	DistractorSlackToken   string
	DistractorInstallation string
	// Failure injection.
	FailCommits int
	MoreHistory bool
	MoreCommits bool
	Truth       groundTruth
}

const (
	evalPrimaryToken        = "xoxb-eval-primary"
	evalPrimaryInstallation = "5001"
)

// changeContextTools are the reads that can serve a commit-caused world: inventory,
// channel reading, and commit history. slack.search_messages is not listed: tool
// availability derives from verified grants, and a bot-token integration is never
// offered user-token-only search, so the model cannot select it at all. Thread and
// pull-request reads join per case, only where the world holds either.
var changeContextTools = []string{
	"slack.list_channels", "slack.get_channel_history",
	"github.list_repositories", "github.read_commits",
}

// evalCases builds the nine §11 archetypes against a shared clock, so every fixture sits
// inside the investigation window the trigger derives (first-seen − 2h → now).
func evalCases(now time.Time) []evalCase {
	deployAt := now.Add(-40 * time.Minute)
	commitAt := now.Add(-45 * time.Minute)

	paymentsWorkspace := func(messages ...evalMessage) map[string]evalWorkspace {
		return map[string]evalWorkspace{evalPrimaryToken: {
			Team: "Acme",
			Channels: []evalChannel{
				{ID: "C1", Name: "incidents", Topic: "live incident chat",
					Messages: messagesIn(messages, "incident")},
				{ID: "C2", Name: "deploys", Topic: "deploy announcements",
					Messages: messagesIn(messages, "deploy")},
			},
			// The directory users.info serves; message authors travel as these raw ids,
			// as the real history API sends them.
			Users: map[string]string{
				"UDEPLOYBOT": "deploy-bot", "USRELEE00": "sre-lee",
				"UDEVROWAN0": "dev-rowan",
			},
		}}
	}

	paymentsRepos := func(commits []evalCommit, pulls []evalPull) map[string]evalInstallation {
		return map[string]evalInstallation{evalPrimaryInstallation: {
			Account: "acme-corp",
			Repos: []evalRepo{
				{ID: 101, Name: "payments", Commits: commits, Pulls: pulls},
				{ID: 102, Name: "deploy", Commits: nil, Pulls: nil},
			},
		}}
	}

	singleCause := evalCase{
		Name:      "single-root-cause",
		Alertname: "CheckoutLatencyHigh",
		Labels:    map[string]string{"namespace": "payments", "service": "payments"},
		Workspaces: paymentsWorkspace(
			deployMessage(deployAt, "UDEPLOYBOT",
				"deployed payments abc123 to production"),
		),
		Installations: paymentsRepos([]evalCommit{
			{SHA: "abc123", Message: "raise connection pool timeout to 30s",
				Author: "kai-dev", At: commitAt},
		}, nil),
		Truth: groundTruth{
			Causes: []causeTruth{{
				Name:    "pool-timeout-commit",
				Markers: []string{"pool timeout", "abc123"},
				Tool:    "github.read_commits",
			}},
			Discriminating: []readTruth{{Tool: "github.read_commits"}},
			RelevantTools:  changeContextTools,
			ExpectFindings: true,
		},
	}

	multiCause := evalCase{
		Name:      "multiple-contributing-causes",
		Alertname: "PaymentsErrorsHigh",
		Labels:    map[string]string{"namespace": "payments", "service": "payments"},
		Workspaces: paymentsWorkspace(
			incidentMessage(now.Add(-25*time.Minute), "USRELEE00",
				"seeing tls handshake errors and connection pool exhaustion at once"),
		),
		Installations: paymentsRepos([]evalCommit{
			{SHA: "def456", Message: "reduce db pool size to 5",
				Author: "kai-dev", At: now.Add(-70 * time.Minute)},
			{SHA: "789abc", Message: "enable strict tls verification on upstreams",
				Author: "ash-ops", At: now.Add(-50 * time.Minute)},
		}, nil),
		Truth: groundTruth{
			Causes: []causeTruth{
				{Name: "pool-size", Markers: []string{"pool size", "def456"},
					Tool: "github.read_commits"},
				{Name: "strict-tls", Markers: []string{"tls", "789abc"},
					Tool: "github.read_commits"},
			},
			Discriminating: []readTruth{{Tool: "github.read_commits"}},
			RelevantTools:  changeContextTools,
			ExpectFindings: true,
		},
	}

	symptomVsCause := evalCase{
		Name:      "symptom-vs-cause",
		Alertname: "CheckoutTimeouts",
		Labels:    map[string]string{"namespace": "checkout", "service": "checkout"},
		Workspaces: paymentsWorkspace(
			incidentMessage(now.Add(-20*time.Minute), "USRELEE00",
				"checkout is timing out for most carts"),
		),
		Installations: map[string]evalInstallation{evalPrimaryInstallation: {
			Account: "acme-corp",
			Repos: []evalRepo{
				{ID: 103, Name: "checkout"},
				{ID: 101, Name: "payments", Commits: []evalCommit{
					{SHA: "cafe01", Message: "drop payments api rate limit to 10 rps",
						Author: "ash-ops", At: commitAt},
				}},
			},
		}},
		Truth: groundTruth{
			Causes: []causeTruth{{
				Name:    "payments-rate-limit",
				Markers: []string{"rate limit", "cafe01"},
				Tool:    "github.read_commits",
			}},
			Discriminating: []readTruth{{Tool: "github.read_commits", ArgMarker: "101"}},
			RelevantTools:  changeContextTools,
			ExpectFindings: true,
		},
	}

	deceptive := evalCase{
		Name:      "deceptive-signal",
		Alertname: "APIGateway5xx",
		Labels:    map[string]string{"namespace": "gateway", "service": "gateway"},
		Workspaces: paymentsWorkspace(
			incidentMessage(now.Add(-22*time.Minute), "UDEVROWAN0",
				"probably the dns migration from yesterday again"),
		),
		Installations: map[string]evalInstallation{evalPrimaryInstallation: {
			Account: "acme-corp",
			Repos: []evalRepo{
				{ID: 104, Name: "gateway", Commits: []evalCommit{
					{SHA: "beef02", Message: "remove retry on upstream 502",
						Author: "kai-dev", At: now.Add(-30 * time.Minute)},
				}},
				{ID: 105, Name: "dns-zones"},
			},
		}},
		Truth: groundTruth{
			Causes: []causeTruth{{
				Name:    "retry-removal",
				Markers: []string{"retry", "beef02"},
				Tool:    "github.read_commits",
			}},
			MustNotClaim:   []string{"dns"},
			Discriminating: []readTruth{{Tool: "github.read_commits", ArgMarker: "104"}},
			RelevantTools:  changeContextTools,
			ExpectFindings: true,
		},
	}

	distractors := singleCause
	distractors.Name = "irrelevant-integration-distractors"
	distractors.DistractorSlackToken = "xoxb-eval-marketing"
	distractors.DistractorInstallation = "7007"
	distractors.Workspaces = map[string]evalWorkspace{
		evalPrimaryToken: singleCause.Workspaces[evalPrimaryToken],
		"xoxb-eval-marketing": {
			Team: "Acme Marketing",
			Channels: []evalChannel{{ID: "C9", Name: "campaigns",
				Topic: "q3 campaign planning", Messages: []evalMessage{
					{At: now.Add(-30 * time.Minute), User: "UMKTJO0000",
						Text: "the newsletter went out on time"},
				}}},
			Users: map[string]string{"UMKTJO0000": "mkt-jo"},
		},
	}
	distractors.Installations = map[string]evalInstallation{
		evalPrimaryInstallation: singleCause.Installations[evalPrimaryInstallation],
		"7007":                  {Account: "acme-web", Repos: []evalRepo{{ID: 300, Name: "website"}}},
	}

	pivot := evalCase{
		Name:      "cross-tool-pivot",
		Alertname: "PaymentsCacheStale",
		Labels:    map[string]string{"namespace": "payments", "service": "payments"},
		Workspaces: paymentsWorkspace(
			deployMessage(now.Add(-35*time.Minute), "UDEPLOYBOT",
				"payments release r2026-08-18-1 rolled out, commit fee1dead"),
		),
		Installations: paymentsRepos([]evalCommit{
			{SHA: "fee1dead", Message: "switch cache to write-through",
				Author: "kai-dev", At: deployAt},
		}, nil),
		Truth: groundTruth{
			Causes: []causeTruth{{
				Name:    "cache-write-through",
				Markers: []string{"write-through", "fee1dead"},
				Tool:    "github.read_commits",
			}},
			Discriminating: []readTruth{
				{Tool: "slack.get_channel_history", ArgMarker: "C2"},
				{Tool: "github.read_commits"},
			},
			RelevantTools:  changeContextTools,
			ExpectFindings: true,
		},
	}

	failedTool := singleCause
	failedTool.Name = "failed-tool-response"
	failedTool.FailCommits = http.StatusInternalServerError
	failedTool.Truth = groundTruth{
		// The commit content is unreachable, so the deploy chatter is the only
		// establishable fact and the commit's content must not be claimed.
		Causes: []causeTruth{{
			Name:    "deploy-observed",
			Markers: []string{"abc123", "deploy"},
			Tool:    "slack.get_channel_history",
		}},
		MustNotClaim:   []string{"pool timeout"},
		Discriminating: []readTruth{{Tool: "github.read_commits"}},
		RelevantTools:  changeContextTools,
		ExpectFindings: true,
	}

	truncated := evalCase{
		Name:      "paginated-truncated-data",
		Alertname: "KafkaConsumerLag",
		Labels:    map[string]string{"namespace": "payments", "service": "payments"},
		Workspaces: paymentsWorkspace(
			incidentMessage(now.Add(-28*time.Minute), "USRELEE00",
				"consumer lag spiked right after the kafka client bump"),
		),
		Installations: paymentsRepos([]evalCommit{
			{SHA: "0ddba11", Message: "bump kafka client to 3.9",
				Author: "ash-ops", At: now.Add(-55 * time.Minute)},
		}, nil),
		MoreHistory: true,
		MoreCommits: true,
		Truth: groundTruth{
			Causes: []causeTruth{{
				Name:    "kafka-client-bump",
				Markers: []string{"kafka client", "0ddba11"},
				Tool:    "github.read_commits",
			}},
			Discriminating: []readTruth{{Tool: "github.read_commits"}},
			RelevantTools:  changeContextTools,
			ExpectFindings: true,
		},
	}

	missing := evalCase{
		Name:       "missing-data-unresolved",
		Alertname:  "NightlyJobFailed",
		Labels:     map[string]string{"namespace": "batch", "service": "batch"},
		Workspaces: paymentsWorkspace(),
		Installations: map[string]evalInstallation{evalPrimaryInstallation: {
			Account: "acme-corp",
			Repos:   []evalRepo{{ID: 106, Name: "batch"}},
		}},
		Truth: groundTruth{
			// Probing an empty world is honest work, whatever is probed.
			RelevantTools: append(append([]string{}, changeContextTools...),
				"slack.get_thread_replies", "github.read_pull_request",
				"github.read_workflow_runs", "github.list_releases"),
			ExpectFindings: false,
		},
	}

	return []evalCase{
		singleCause, multiCause, symptomVsCause, deceptive, distractors,
		pivot, failedTool, truncated, missing,
	}
}

// deployMessage and incidentMessage tag fixtures for the channel split in
// paymentsWorkspace.
func deployMessage(at time.Time, user, text string) evalMessage {
	return evalMessage{At: at, User: user, Text: "[deploy] " + text}
}

func incidentMessage(at time.Time, user, text string) evalMessage {
	return evalMessage{At: at, User: user, Text: "[incident] " + text}
}

func messagesIn(messages []evalMessage, kind string) []evalMessage {
	var inside []evalMessage
	for _, message := range messages {
		if strings.HasPrefix(message.Text, "["+kind+"] ") {
			message.Text = strings.TrimPrefix(message.Text, "["+kind+"] ")
			inside = append(inside, message)
		}
	}
	return inside
}
