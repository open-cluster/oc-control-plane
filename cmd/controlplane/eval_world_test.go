package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The evaluation harness's incident worlds: scripted Slack and GitHub vendors, richer
// than the per-feature fakes in slack_test.go and github_test.go because an
// investigation walks several tools across one consistent story. Fixture state is
// immutable once a world is built; the only mutability is failure injection, declared
// by the case.

// evalMessage is one scripted Slack message.
type evalMessage struct {
	At   time.Time
	User string
	Text string
	// ThreadTS marks this message as heading a thread when replies exist under its stamp.
	ThreadTS   string
	ReplyCount int
}

// evalChannel is one scripted Slack channel with its messages.
type evalChannel struct {
	ID       string
	Name     string
	Topic    string
	Messages []evalMessage
}

// evalWorkspace is what one Slack token sees.
type evalWorkspace struct {
	Team     string
	Channels []evalChannel
}

// evalSlackFake serves the Slack Web API surface the tools read, per token, so a
// distractor integration created with its own token sees its own workspace.
type evalSlackFake struct {
	*httptest.Server

	mu         sync.Mutex
	workspaces map[string]evalWorkspace
	// moreHistory forces has_more on history answers, for the truncated-data case.
	moreHistory bool
}

func newEvalSlackFake(t *testing.T, workspaces map[string]evalWorkspace) *evalSlackFake {
	t.Helper()
	fake := &evalSlackFake{workspaces: workspaces}
	fake.Server = httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(fake.Close)
	return fake
}

func (f *evalSlackFake) serve(writer http.ResponseWriter, request *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	writer.Header().Set("Content-Type", "application/json")
	token := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
	workspace, known := f.workspaces[token]
	if !known {
		_, _ = writer.Write([]byte(`{"ok":false,"error":"invalid_auth"}`))
		return
	}

	query := request.URL.Query()
	switch request.URL.Path {
	case "/auth.test":
		writer.Header().Set("X-OAuth-Scopes", "channels:read,channels:history,search:read")
		evalAnswer(writer, map[string]any{"ok": true, "team": workspace.Team, "user": "opencluster-bot"})
	case "/conversations.list":
		channels := make([]map[string]any, 0, len(workspace.Channels))
		for _, channel := range workspace.Channels {
			channels = append(channels, map[string]any{
				"id": channel.ID, "name": channel.Name,
				"topic":       map[string]any{"value": channel.Topic},
				"purpose":     map[string]any{"value": ""},
				"num_members": 5,
			})
		}
		evalAnswer(writer, map[string]any{"ok": true, "channels": channels})
	case "/conversations.history":
		channel, found := channelByID(workspace, query.Get("channel"))
		if !found {
			_, _ = writer.Write([]byte(`{"ok":false,"error":"channel_not_found"}`))
			return
		}
		messages := windowed(channel.Messages, query.Get("oldest"), query.Get("latest"))
		evalAnswer(writer, map[string]any{
			"ok": true, "has_more": f.moreHistory, "messages": renderMessages(messages),
		})
	case "/conversations.replies":
		channel, found := channelByID(workspace, query.Get("channel"))
		if !found {
			_, _ = writer.Write([]byte(`{"ok":false,"error":"channel_not_found"}`))
			return
		}
		var replies []evalMessage
		for _, message := range channel.Messages {
			if message.ThreadTS == query.Get("ts") {
				replies = append(replies, message)
			}
		}
		evalAnswer(writer, map[string]any{"ok": true, "messages": renderMessages(replies)})
	case "/search.messages":
		// Real Slack refuses classic search to a bot token (issue #5 §15); a world
		// that answered it would flatter the baseline down a path production never has.
		_, _ = writer.Write([]byte(`{"ok":false,"error":"not_allowed_token_type"}`))
	default:
		_, _ = writer.Write([]byte(`{"ok":false,"error":"unknown_method"}`))
	}
}

func channelByID(workspace evalWorkspace, id string) (evalChannel, bool) {
	for _, channel := range workspace.Channels {
		if channel.ID == id {
			return channel, true
		}
	}
	return evalChannel{}, false
}

// windowed filters messages by Slack's decimal-seconds bounds, as the real API does.
func windowed(messages []evalMessage, oldest, latest string) []evalMessage {
	after, before := slackBound(oldest), slackBound(latest)
	var inside []evalMessage
	for _, message := range messages {
		if !after.IsZero() && message.At.Before(after) {
			continue
		}
		if !before.IsZero() && message.At.After(before) {
			continue
		}
		inside = append(inside, message)
	}
	return inside
}

func slackBound(stamp string) time.Time {
	if stamp == "" {
		return time.Time{}
	}
	seconds, err := strconv.ParseFloat(stamp, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(int64(seconds), 0)
}

func renderMessages(messages []evalMessage) []map[string]any {
	rendered := make([]map[string]any, 0, len(messages))
	for _, message := range messages {
		one := map[string]any{
			"ts":   slackStamp(message.At),
			"user": message.User,
			"text": message.Text,
		}
		if message.ThreadTS != "" {
			one["thread_ts"] = message.ThreadTS
		}
		if message.ReplyCount > 0 {
			one["reply_count"] = message.ReplyCount
		}
		rendered = append(rendered, one)
	}
	return rendered
}

func slackStamp(at time.Time) string {
	return strconv.FormatInt(at.Unix(), 10) + ".000100"
}

func evalAnswer(writer http.ResponseWriter, payload map[string]any) {
	encoded, _ := json.Marshal(payload)
	_, _ = writer.Write(encoded)
}

// evalCommit is one scripted commit.
type evalCommit struct {
	SHA     string
	Message string
	Author  string
	At      time.Time
}

// evalPull is one scripted pull request.
type evalPull struct {
	Number   int
	Title    string
	State    string
	MergedAt time.Time
	Author   string
}

// evalRepo is one scripted repository with its history.
type evalRepo struct {
	ID      int64
	Name    string
	Commits []evalCommit
	Pulls   []evalPull
}

// evalInstallation is what one GitHub App installation granted.
type evalInstallation struct {
	Account string
	Repos   []evalRepo
}

// evalGitHubFake serves the GitHub REST surface the tools read, per installation, so a
// distractor integration configured with its own installation id sees its own grant.
// Minted tokens encode the installation so later reads resolve the right grant.
type evalGitHubFake struct {
	*httptest.Server

	mu            sync.Mutex
	installations map[string]evalInstallation
	// failCommits answers every commits read with this HTTP status when non-zero.
	failCommits int
	// moreCommits adds a next-page Link header to commits answers.
	moreCommits bool
}

func newEvalGitHubFake(t *testing.T, installations map[string]evalInstallation) *evalGitHubFake {
	t.Helper()
	fake := &evalGitHubFake{installations: installations}
	fake.Server = httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(fake.Close)
	return fake
}

func (f *evalGitHubFake) serve(writer http.ResponseWriter, request *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	writer.Header().Set("Content-Type", "application/json")
	path := request.URL.Path

	if installation, isApp := strings.CutPrefix(path, "/app/installations/"); isApp {
		id, minting := strings.CutSuffix(installation, "/access_tokens")
		granted, known := f.installations[id]
		if !known {
			writer.WriteHeader(http.StatusNotFound)
			_, _ = writer.Write([]byte(`{"message":"Not Found"}`))
			return
		}
		if minting {
			writer.WriteHeader(http.StatusCreated)
			_, _ = writer.Write([]byte(`{"token":"ghs_` + id +
				`","expires_at":"2036-01-01T00:00:00Z"}`))
			return
		}
		_, _ = writer.Write([]byte(`{"id":` + id + `,"account":{"login":"` + granted.Account +
			`","type":"Organization"},"repository_selection":"selected","suspended_at":null}`))
		return
	}

	token := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
	granted, known := f.installations[strings.TrimPrefix(token, "ghs_")]
	if !known {
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = writer.Write([]byte(`{"message":"Bad credentials"}`))
		return
	}

	switch {
	case path == "/installation/repositories":
		repos := make([]map[string]any, 0, len(granted.Repos))
		for _, repo := range granted.Repos {
			repos = append(repos, map[string]any{
				"id": repo.ID, "name": repo.Name,
				"full_name": granted.Account + "/" + repo.Name,
			})
		}
		evalAnswer(writer, map[string]any{"total_count": len(repos), "repositories": repos})
	case strings.HasSuffix(path, "/commits"):
		if f.failCommits != 0 {
			writer.WriteHeader(f.failCommits)
			_, _ = writer.Write([]byte(`{"message":"Server Error"}`))
			return
		}
		repo, found := repoByPath(granted, path, "/commits")
		if !found {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		if f.moreCommits {
			writer.Header().Set("Link", `<`+f.URL+path+`?page=2>; rel="next"`)
		}
		writeList(writer, renderCommits(repo.Commits, request.URL.Query()))
	case strings.HasSuffix(path, "/pulls"):
		repo, found := repoByPath(granted, path, "/pulls")
		if !found {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writeList(writer, renderPulls(repo.Pulls))
	default:
		writer.WriteHeader(http.StatusNotFound)
		_, _ = writer.Write([]byte(`{"message":"Not Found"}`))
	}
}

func repoByPath(granted evalInstallation, path, suffix string) (evalRepo, bool) {
	id := strings.TrimSuffix(strings.TrimPrefix(path, "/repositories/"), suffix)
	for _, repo := range granted.Repos {
		if strconv.FormatInt(repo.ID, 10) == id {
			return repo, true
		}
	}
	return evalRepo{}, false
}

func renderCommits(commits []evalCommit, query url.Values) []map[string]any {
	since, _ := time.Parse(time.RFC3339, query.Get("since"))
	until, _ := time.Parse(time.RFC3339, query.Get("until"))
	rendered := make([]map[string]any, 0, len(commits))
	for _, commit := range commits {
		if !since.IsZero() && commit.At.Before(since) {
			continue
		}
		if !until.IsZero() && commit.At.After(until) {
			continue
		}
		rendered = append(rendered, map[string]any{
			"sha": commit.SHA,
			"commit": map[string]any{
				"message": commit.Message,
				"author": map[string]any{
					"name": commit.Author, "date": commit.At.UTC().Format(time.RFC3339),
				},
			},
			"author": map[string]any{"login": commit.Author},
		})
	}
	return rendered
}

func renderPulls(pulls []evalPull) []map[string]any {
	rendered := make([]map[string]any, 0, len(pulls))
	for _, pull := range pulls {
		one := map[string]any{
			"number": pull.Number, "title": pull.Title, "state": pull.State,
			"updated_at": pull.MergedAt.UTC().Format(time.RFC3339),
			"user":       map[string]any{"login": pull.Author},
			"head":       map[string]any{"ref": "change"}, "base": map[string]any{"ref": "main"},
		}
		if !pull.MergedAt.IsZero() {
			one["merged_at"] = pull.MergedAt.UTC().Format(time.RFC3339)
		}
		rendered = append(rendered, one)
	}
	return rendered
}

func writeList(writer http.ResponseWriter, list []map[string]any) {
	encoded, _ := json.Marshal(list)
	_, _ = writer.Write(encoded)
}
