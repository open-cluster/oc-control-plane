package main

import "time"

// THE PEACETIME AND LONG-CONVERSATION WORLDS, per issue #12 stories 40 and 41.
//
// The nine incident archetypes beside these all begin with something being wrong. Most of
// what an SRE actually asks is not that: which revision is live, who owns this, when did
// this last change. Those have a right answer and no cause to name, and an agent measured
// only on incidents is an agent nobody has measured on the ordinary case.
//
// Each world holds the evidence its answer needs, a deliberate distractor integration whose
// reads are irrelevant by construction, and an answer whose markers a scorer can check
// without judgement. The truth is answer-shaped rather than cause-shaped: what matters is
// the reply, because a question's deliverable IS its reply.

// evalConversationCases builds the conversation-triggered worlds against the same clock the
// incident cases use, so every fixture sits inside the window a turn derives.
func evalConversationCases(now time.Time) []evalCase {
	deployedAt := now.Add(-20 * time.Minute)
	supersededAt := now.Add(-100 * time.Minute)
	changedAt := now.Add(-45 * time.Minute)

	// The distractor worlds. Real, readable, and irrelevant by construction: reads against
	// them are counted against the run rather than serving it.
	marketing := evalWorkspace{
		Team: "Acme Marketing",
		Channels: []evalChannel{{ID: "C9", Name: "campaigns",
			Topic: "q3 campaign planning", Messages: []evalMessage{
				{At: now.Add(-30 * time.Minute), User: "UMKTJO0000",
					Text: "the autumn newsletter went out, version v9.0.0 of the template"},
			}}},
		Users: map[string]string{"UMKTJO0000": "mkt-jo"},
	}
	website := evalInstallation{
		Account: "acme-web",
		Repos: []evalRepo{{ID: 300, Name: "website", Files: map[string]string{
			"CODEOWNERS": "/site/ @acme-web/web-guild\n",
		}}},
	}

	deployWorkspace := func(messages ...evalMessage) map[string]evalWorkspace {
		return map[string]evalWorkspace{evalPrimaryToken: {
			Team: "Acme",
			Channels: []evalChannel{
				{ID: "C1", Name: "incidents", Topic: "live incident chat"},
				{ID: "C2", Name: "deploys", Topic: "deploy announcements",
					Messages: messagesIn(messages, "deploy")},
			},
			Users: map[string]string{
				"UDEPLOYBOT": "deploy-bot", "USRELEE00": "sre-lee",
				"UDEVROWAN0": "dev-rowan",
			},
		}}
	}

	// WHICH REVISION IS LIVE. The superseded announcement is the trap: it is real, it is
	// in the window, and it is wrong. Reading the channel is not enough — the answer
	// depends on reading it in order.
	whichRevision := evalCase{
		Name:     "peacetime-which-revision-is-deployed",
		Question: "which revision of payments is running in production right now?",
		Workspaces: withMarketing(deployWorkspace(
			deployMessage(supersededAt, "UDEPLOYBOT",
				"deployed payments v2.13.9 to production"),
			deployMessage(deployedAt, "UDEPLOYBOT",
				"deployed payments v2.14.1 to production"),
		), marketing),
		Installations: map[string]evalInstallation{evalPrimaryInstallation: {
			Account: "acme-corp",
			Repos:   []evalRepo{{ID: 101, Name: "payments"}},
		}},
		DistractorSlackToken: "xoxb-eval-marketing",
		Truth: groundTruth{
			AnswerMarkers: []string{"v2.14.1"},
			// The superseded revision, announced earlier in the same channel. It is what
			// reading the deploy channel out of order produces, which is the exact
			// mistake this world was built to punish.
			MustNotAnswer: []string{"v2.13.9"},
			Discriminating: []readTruth{
				{Tool: "slack.get_channel_history", ArgMarker: "C2"},
			},
			RelevantTools: []string{
				"slack.list_channels", "slack.get_channel_history",
				"github.list_repositories",
			},
			ExpectFindings: true,
		},
	}

	// WHO OWNS IT. The answer is in a file, not in chat, so a run that only reads Slack
	// cannot get there. The channel topic names a team that does not own the service,
	// which is what makes the file read discriminating rather than decorative.
	whoOwns := evalCase{
		Name:     "peacetime-who-owns-the-service",
		Question: "which team owns the payments service?",
		Workspaces: map[string]evalWorkspace{evalPrimaryToken: {
			Team: "Acme",
			Channels: []evalChannel{
				{ID: "C1", Name: "payments-chat", Topic: "run by the on-call rota"},
			},
			Users: map[string]string{"USRELEE00": "sre-lee"},
		}},
		Installations: map[string]evalInstallation{
			evalPrimaryInstallation: {
				Account: "acme-corp",
				Repos: []evalRepo{{ID: 101, Name: "payments", Files: map[string]string{
					"CODEOWNERS": "/payments/ @acme-corp/payments-platform\n" +
						"/docs/ @acme-corp/docs-guild\n",
				}}},
			},
			"5002": website,
		},
		DistractorInstallation: "5002",
		Truth: groundTruth{
			AnswerMarkers: []string{"payments-platform"},
			// Two wrong owners, one in the distractor repository and one in the same
			// CODEOWNERS file under a different path. An ownership answer that hedges
			// between the right team and either of them has not answered the question.
			MustNotAnswer: []string{"web-guild", "docs-guild"},
			Discriminating: []readTruth{
				{Tool: "github.read_file", ArgMarker: "CODEOWNERS"},
			},
			RelevantTools: []string{
				"github.list_repositories", "github.read_file",
				"slack.list_channels", "slack.get_channel_history",
			},
			ExpectFindings: true,
		},
	}

	// WHEN DID IT LAST CHANGE. A question about history, answered from history, with a
	// second commit to the same repository that did not touch the file in question.
	whenChanged := evalCase{
		Name: "peacetime-when-did-this-last-change",
		Question: "when did the payments connection pool configuration last change, " +
			"and who changed it?",
		Workspaces: withMarketing(deployWorkspace(), marketing),
		Installations: map[string]evalInstallation{evalPrimaryInstallation: {
			Account: "acme-corp",
			Repos: []evalRepo{{ID: 101, Name: "payments", Commits: []evalCommit{
				{SHA: "abc123", Message: "raise connection pool timeout to 30s",
					Author: "kai-dev", At: changedAt,
					Files: []evalChange{{Path: "config/pool.yaml",
						Patch: "@@ -1,3 +1,3 @@\n pool:\n-  connect_timeout: 2s\n+  connect_timeout: 30s"}}},
				{SHA: "fed321", Message: "update the readme",
					Author: "dev-rowan", At: now.Add(-30 * time.Minute),
					Files: []evalChange{{Path: "README.md",
						Patch: "@@ -1 +1 @@\n-payments\n+payments service"}}},
			}, Files: map[string]string{
				"config/pool.yaml": "pool:\n  connect_timeout: 30s\n  max_connections: 40\n",
			}}},
		}},
		DistractorSlackToken: "xoxb-eval-marketing",
		Truth: groundTruth{
			// The question asks WHEN it changed and WHO changed it. Marking only the
			// commit scored a third of an answer as a whole one, so the author is marked
			// too. The date is deliberately NOT marked: it is derived from the run's own
			// clock and a model may write it a dozen defensible ways, so a marker would
			// produce false negatives. That clause is the rubric layer's to judge, and
			// this comment is here so its absence reads as a decision rather than a gap.
			AnswerMarkers: []string{"abc123", "kai-dev"},
			// The readme commit and its author: the most recent change to the repository,
			// and the wrong answer to a question about the pool CONFIGURATION.
			MustNotAnswer:  []string{"fed321", "dev-rowan"},
			Discriminating: []readTruth{{Tool: "github.read_commits"}},
			RelevantTools: []string{
				"github.list_repositories", "github.read_commits", "github.read_commit",
				"github.read_file", "slack.list_channels", "slack.get_channel_history",
			},
			ExpectFindings: true,
		},
	}

	// THE LONG CONVERSATION. Turn one establishes the fact the whole case is about, turn
	// two states a constraint and adds what it can see, and the turns after that are an
	// operator thinking out loud at the length operators actually write. The last turn
	// asks for the fact again, by which point the conversation has been compacted.
	//
	// SIZING, because none of these numbers is arbitrary and the constraint on them is
	// tighter than it looks. ONE number is both the compaction trigger and the turn's own
	// transcript ceiling. Compaction fires when the held context alone passes it; the
	// ceiling fires when the held context PLUS the tool catalogue passes it. The catalogue
	// is never zero, so the ceiling is always reached first, and the turn that compacts is
	// always a turn that is then told to conclude from what it has.
	//
	// That is the shape this world is built around rather than against, because it is
	// exactly the moment worth testing: the last turn answers from memory, having read
	// nothing. So the reads live in the EARLY turns, while the thread is short, and the
	// long messages arrive afterwards to push the held context past the line. The world
	// connects Slack alone — a vendor whose data it does not hold would cost every turn a
	// whole catalogue to offer reads that answer nothing.
	longConversation := evalCase{
		Name:     "conversation-memory-across-compaction",
		Question: "what changed in payments shortly before the latency rose?",
		FollowUps: []string{
			"ignore the database for now, stay on deployments",
			"here is the shape of it. the p99 on checkout went from 240ms to 3.1s " +
				"between 14:00 and 14:10 and has stayed there since. the p50 barely " +
				"moved, maybe 12ms to 15ms, which is what makes me think this is a pool " +
				"or a lock rather than raw load, because raw load moves the whole " +
				"distribution and this only moved the tail. request volume over the same " +
				"window was flat within a few percent of the previous hour, so nothing " +
				"suggests a traffic spike, and the upstream cdn reports no change in its " +
				"cache hit ratio either. the error rate is low, under one percent, and " +
				"the errors that are there are timeouts rather than refusals, which " +
				"again points at waiting rather than at capacity being turned away. be " +
				"careful with the phrase 'lines up' when you answer: i want the " +
				"ordering, not a correlation inferred from two things both being recent.",
			"one more thing worth knowing before you go further. we rolled the same pool " +
				"change into staging about two hours earlier and saw nothing at all. " +
				"staging runs at maybe a fiftieth of production traffic and has its own " +
				"database with a much smaller connection ceiling, so a saturation effect " +
				"there would not be visible even if the change is what causes it here. " +
				"do not treat the quiet staging run as evidence that the change is safe. " +
				"it is evidence that we did not reproduce it, which is a different claim " +
				"and a much weaker one, and i have watched people talk themselves out of " +
				"a correct diagnosis on exactly that reasoning more than once. if you " +
				"mention staging at all, mention it as an absence of a reproduction.",
			"the other thing i keep going back and forth on is the cache. the warmers " +
				"restart on every deployment and take a few minutes to repopulate, and " +
				"during that window everything downstream reads through to the database " +
				"instead of being served from memory. if the warmers were slow this time " +
				"then some of the latency is the warm-up rather than the pool change, " +
				"and i would like to know which part is which before we decide whether " +
				"to roll back, because rolling back the pool change does nothing for a " +
				"warm-up problem and costs us another deployment window on a service " +
				"that has already had two today.",
			"for the record, we are not going to roll back tonight either way. the " +
				"on-call rota changes at eight and i would rather the person picking " +
				"this up has a written answer than an in-flight deployment. so treat " +
				"this as a diagnosis exercise rather than a remediation one, and when " +
				"you summarise, summarise for somebody who has not read any of the " +
				"messages above and is starting from the beginning with whatever you " +
				"leave them.",
			"what did we establish about the deploy? name the commit.",
		},
		// A 32k window reserves 16k for the answer, and 7% of what is left is 1,120
		// tokens. The early turns orient at roughly 850 and have room to read; the held
		// context passes 1,120 around the fifth turn and is compacted from there on.
		ContextWindow:           32_000,
		ContextThresholdPercent: 7,
		Workspaces: deployWorkspace(
			deployMessage(changedAt, "UDEPLOYBOT",
				"deployed payments abc123 to production, pool timeout 2s -> 30s"),
			deployMessage(deployedAt, "UDEVROWAN0",
				"cache warmers restarted after the payments deploy"),
		),
		Truth: groundTruth{
			// The fact the first turn established, asked for again at the end.
			Survives:         []string{"abc123"},
			AnswerMarkers:    []string{"abc123"},
			ExpectCompaction: true,
			Discriminating: []readTruth{
				{Tool: "slack.get_channel_history", ArgMarker: "C2"},
			},
			RelevantTools: []string{
				"slack.list_channels", "slack.get_channel_history",
			},
			ExpectFindings: true,
		},
	}

	return []evalCase{whichRevision, whoOwns, whenChanged, longConversation}
}

// withMarketing adds the distractor workspace beside the one a case is meant to read.
func withMarketing(
	workspaces map[string]evalWorkspace, marketing evalWorkspace,
) map[string]evalWorkspace {
	workspaces["xoxb-eval-marketing"] = marketing
	return workspaces
}
