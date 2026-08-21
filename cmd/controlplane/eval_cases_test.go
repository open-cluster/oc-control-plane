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
// carries any marker AND cites a run of one of the named tools — a statement without
// provenance is not a found cause. Tools is a set because sound provenance has more
// than one spelling: the commit listing that surfaced a change and the diff read that
// verified it are both the change's evidence.
type causeTruth struct {
	Name    string
	Markers []string
	Tools   []string
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

	// MustNotAnswer are the world's OWN plausible wrong values — the revision that is not
	// deployed, the team that does not own the service. A world builds them in to punish
	// reading its evidence carelessly, so an answer asserting one is a false claim rather
	// than a partial answer. Naming one in order to rule it out is not.
	MustNotAnswer []string
	// AnswerMarkers are what the direct reply must carry. A question's deliverable is
	// its answer, so these are checked against the answer text and nothing else.
	AnswerMarkers []string
	// Survives are facts established early in a long conversation that must still be
	// there at the end of it. Checked against the LAST turn only: finding them anywhere
	// would find them in the turn that established them, which proves nothing.
	Survives []string
	// ExpectCompaction marks a world built to outgrow its context window. A case that
	// expected one and did not get one says nothing about memory, and is reported so.
	ExpectCompaction bool
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

	// Question makes the case a CONVERSATION rather than an incident: it is asked
	// through the operator surface instead of an alert being delivered, and there is no
	// episode. Empty leaves the case alert-triggered.
	Question string
	// FollowUps are asked in order after the first turn ends, one turn each.
	FollowUps []string
	// ContextWindow is the model window this case's deployment declares, and
	// ContextThresholdPercent is how full it may get before older turns are compacted.
	// Together they are how a long conversation is made to compact on a modest transcript
	// rather than on a bought one. Zero leaves the deployment default.
	ContextWindow           int
	ContextThresholdPercent int

	Truth groundTruth
}

const (
	evalPrimaryToken        = "xoxb-eval-primary"
	evalPrimaryInstallation = "5001"
)

// changeContextTools are the reads that serve a commit-caused world's causal workflow:
// inventory, channel reading, commit history, the suspect commit's own diff, and the
// cheap rule-outs (CI runs, releases) whose negative answers the ground truth rewards.
// slack.search_messages is not listed: tool availability derives from verified grants,
// and a bot-token integration is never offered user-token-only search, so the model
// cannot select it at all. Thread, pull-request and file reads join per case, only
// where the world holds either.
var changeContextTools = []string{
	"slack.list_channels", "slack.get_channel_history",
	"github.list_repositories", "github.read_commits", "github.read_commit",
	"github.read_workflow_runs", "github.list_releases",
}

// fileContextTools extends the change-context set for the worlds whose commits name
// readable files: there, reading the configuration a diff touched is the mechanism
// verification the causal workflow ends on.
var fileContextTools = append(
	append([]string{}, changeContextTools...), "github.read_file")

// commitEvidence is the provenance a commit-caused finding may cite: the listing that
// surfaced the change, the diff that verified it, or the file it touched.
var commitEvidence = []string{
	"github.read_commits", "github.read_commit", "github.read_file",
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

	paymentsRepos := func(
		commits []evalCommit, pulls []evalPull, files map[string]string,
	) map[string]evalInstallation {
		return map[string]evalInstallation{evalPrimaryInstallation: {
			Account: "acme-corp",
			Repos: []evalRepo{
				{ID: 101, Name: "payments", Commits: commits, Pulls: pulls, Files: files},
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
				Author: "kai-dev", At: commitAt,
				Files: []evalChange{{Path: "config/pool.yaml",
					Patch: "@@ -1,3 +1,3 @@\n pool:\n-  connect_timeout: 2s\n+  connect_timeout: 30s"}}},
		}, nil, map[string]string{
			"config/pool.yaml": "pool:\n  connect_timeout: 30s\n  max_connections: 40\n",
		}),
		Truth: groundTruth{
			Causes: []causeTruth{{
				Name:    "pool-timeout-commit",
				Markers: []string{"pool timeout", "abc123"},
				Tools:   commitEvidence,
			}},
			Discriminating: []readTruth{{Tool: "github.read_commits"}},
			RelevantTools:  fileContextTools,
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
				Author: "kai-dev", At: now.Add(-70 * time.Minute),
				Files: []evalChange{{Path: "config/db.yaml",
					Patch: "@@ -1,3 +1,3 @@\n db:\n-  pool_size: 50\n+  pool_size: 5"}}},
			{SHA: "789abc", Message: "enable strict tls verification on upstreams",
				Author: "ash-ops", At: now.Add(-50 * time.Minute),
				Files: []evalChange{{Path: "config/upstreams.yaml",
					Patch: "@@ -1,3 +1,3 @@\n upstreams:\n-  tls_verify: off\n+  tls_verify: strict"}}},
		}, nil, map[string]string{
			"config/db.yaml":        "db:\n  pool_size: 5\n  host: payments-db\n",
			"config/upstreams.yaml": "upstreams:\n  tls_verify: strict\n  timeout: 5s\n",
		}),
		Truth: groundTruth{
			Causes: []causeTruth{
				{Name: "pool-size", Markers: []string{"pool size", "def456"},
					Tools: commitEvidence},
				{Name: "strict-tls", Markers: []string{"tls", "789abc"},
					Tools: commitEvidence},
			},
			Discriminating: []readTruth{{Tool: "github.read_commits"}},
			RelevantTools:  fileContextTools,
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
						Author: "ash-ops", At: commitAt,
						Files: []evalChange{{Path: "config/ratelimit.yaml",
							Patch: "@@ -1,3 +1,3 @@\n api:\n-  rate_limit_rps: 500\n+  rate_limit_rps: 10"}}},
				}, Files: map[string]string{
					"config/ratelimit.yaml": "api:\n  rate_limit_rps: 10\n  burst: 20\n",
				}},
			},
		}},
		Truth: groundTruth{
			Causes: []causeTruth{{
				Name:    "payments-rate-limit",
				Markers: []string{"rate limit", "cafe01"},
				Tools:   commitEvidence,
			}},
			Discriminating: []readTruth{{Tool: "github.read_commits", ArgMarker: "101"}},
			RelevantTools:  fileContextTools,
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
						Author: "kai-dev", At: now.Add(-30 * time.Minute),
						Files: []evalChange{{Path: "config/gateway.yaml",
							Patch: "@@ -1,3 +1,3 @@\n upstream:\n-  retry_on_502: true\n+  retry_on_502: false"}}},
				}, Files: map[string]string{
					"config/gateway.yaml": "upstream:\n  retry_on_502: false\n  timeout: 10s\n",
				}},
				{ID: 105, Name: "dns-zones"},
			},
		}},
		Truth: groundTruth{
			Causes: []causeTruth{{
				Name:    "retry-removal",
				Markers: []string{"retry", "beef02"},
				Tools:   commitEvidence,
			}},
			MustNotClaim:   []string{"dns"},
			Discriminating: []readTruth{{Tool: "github.read_commits", ArgMarker: "104"}},
			RelevantTools:  fileContextTools,
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
				Author: "kai-dev", At: deployAt,
				Files: []evalChange{{Path: "config/cache.yaml",
					Patch: "@@ -1,3 +1,3 @@\n cache:\n-  mode: write-back\n+  mode: write-through"}}},
		}, nil, map[string]string{
			"config/cache.yaml": "cache:\n  mode: write-through\n  ttl: 300s\n",
		}),
		Truth: groundTruth{
			Causes: []causeTruth{{
				Name:    "cache-write-through",
				Markers: []string{"write-through", "fee1dead"},
				Tools:   commitEvidence,
			}},
			Discriminating: []readTruth{
				{Tool: "slack.get_channel_history", ArgMarker: "C2"},
				{Tool: "github.read_commits"},
			},
			RelevantTools:  fileContextTools,
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
			Tools:   []string{"slack.get_channel_history"},
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
				Author: "ash-ops", At: now.Add(-55 * time.Minute),
				Files: []evalChange{{Path: "go.mod",
					Patch: "@@ -3,3 +3,3 @@\n require (\n-\tacme.dev/kafka-client v3.8.0\n+\tacme.dev/kafka-client v3.9.0"}}},
		}, nil, map[string]string{
			"go.mod": "module acme.dev/payments\n\nrequire (\n\tacme.dev/kafka-client v3.9.0\n)\n",
		}),
		MoreHistory: true,
		MoreCommits: true,
		Truth: groundTruth{
			Causes: []causeTruth{{
				Name:    "kafka-client-bump",
				Markers: []string{"kafka client", "0ddba11"},
				Tools:   commitEvidence,
			}},
			Discriminating: []readTruth{{Tool: "github.read_commits"}},
			RelevantTools:  fileContextTools,
			ExpectFindings: true,
		},
	}

	// The anti-overfit twin of the timing principle: the causal commit and its deploy
	// PRECEDE the alert, and a post-onset remediation commit with its own deploy chatter
	// tempts misattribution. An investigator that learned only "the commit nearest the
	// alert wins" — or "any post-onset commit is innocent, any pre-onset commit guilty,
	// no mechanism needed" — fails here; the judge grades the causal direction.
	remediation := evalCase{
		Name:      "remediation-red-herring",
		Alertname: "QueueBacklogGrowing",
		Labels:    map[string]string{"namespace": "payments", "service": "worker"},
		Workspaces: paymentsWorkspace(
			deployMessage(now.Add(-40*time.Minute), "UDEPLOYBOT",
				"deployed worker bad111 to production"),
			incidentMessage(now.Add(-6*time.Minute), "USRELEE00",
				"restoring consumer concurrency to drain the backlog"),
			deployMessage(now.Add(-5*time.Minute), "UDEPLOYBOT",
				"deployed worker aid222 to production"),
		),
		Installations: paymentsRepos([]evalCommit{
			{SHA: "bad111", Message: "reduce worker consumer concurrency to 1",
				Author: "kai-dev", At: now.Add(-45 * time.Minute),
				Files: []evalChange{{Path: "config/worker.yaml",
					Patch: "@@ -1,3 +1,3 @@\n worker:\n-  consumer_concurrency: 16\n+  consumer_concurrency: 1"}}},
			{SHA: "aid222", Message: "restore worker consumer concurrency to 16",
				Author: "ash-ops", At: now.Add(-7 * time.Minute),
				Files: []evalChange{{Path: "config/worker.yaml",
					Patch: "@@ -1,3 +1,3 @@\n worker:\n-  consumer_concurrency: 1\n+  consumer_concurrency: 16"}}},
		}, nil, map[string]string{
			"config/worker.yaml": "worker:\n  consumer_concurrency: 16\n  queue: payments-events\n",
		}),
		Truth: groundTruth{
			Causes: []causeTruth{{
				Name:    "concurrency-drop",
				Markers: []string{"concurrency", "bad111"},
				Tools:   commitEvidence,
			}},
			Discriminating: []readTruth{{Tool: "github.read_commits"}},
			RelevantTools:  fileContextTools,
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

	return append([]evalCase{
		singleCause, multiCause, symptomVsCause, deceptive, distractors,
		pivot, failedTool, truncated, missing, remediation,
	}, evalConversationCases(now)...)
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
