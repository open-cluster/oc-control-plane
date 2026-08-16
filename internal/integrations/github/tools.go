package github

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// The four read-only tools. Repositories are addressed by their stable numeric ids —
// never by name, which a rename would break mid-incident — and every bound is named, with
// truncation flagged from the vendor's own pagination.

// The named bounds. GitHub's page ceiling is 100 everywhere; the defaults are sized for
// an investigation reading change context, not for export.
const (
	maxItemsPerRead     = 100
	defaultRepositories = 50
	defaultCommits      = 30
	defaultPullRequests = 20
)

// rateLimitNote is the same fact on every tool, stated once.
const rateLimitNote = "one GitHub REST call per invocation, against the installation's " +
	"shared hourly budget; a handful of calls per investigation is fine, a crawl is not"

// tools is the declared set, one-to-one with the capabilities the definition declares.
func tools(app *App, client *Client) []integrations.Tool {
	return []integrations.Tool{
		listRepositoriesTool(app, client),
		readRepositoryTool(app, client),
		readCommitsTool(app, client),
		readPullRequestsTool(app, client),
	}
}

// repositoryContent is one repository as a tool reports it.
type repositoryContent struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	FullName      string `json:"fullName"`
	Private       bool   `json:"private"`
	Archived      bool   `json:"archived"`
	DefaultBranch string `json:"defaultBranch,omitempty"`
	Description   string `json:"description,omitempty"`
}

// commitContent is one commit as a tool reports it.
type commitContent struct {
	SHA      string `json:"sha"`
	Message  string `json:"message"`
	Author   string `json:"author,omitempty"`
	AuthorAt string `json:"authorAt,omitempty"`
}

// pullRequestContent is one pull request as a tool reports it.
type pullRequestContent struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	State     string `json:"state"`
	MergedAt  string `json:"mergedAt,omitempty"`
	UpdatedAt string `json:"updatedAt"`
	Author    string `json:"author,omitempty"`
	Head      string `json:"head,omitempty"`
	Base      string `json:"base,omitempty"`
}

func listRepositoriesTool(app *App, client *Client) integrations.Tool {
	declared := []integrations.ToolArgument{
		{
			Name: "limit",
			Description: fmt.Sprintf("How many repositories to return, at most %d. Default %d.",
				maxItemsPerRead, defaultRepositories),
			Type: integrations.FieldInteger,
		},
	}
	return integrations.Tool{
		Name:       "github.list_repositories",
		Capability: ListRepositories,
		Description: "Lists the repositories this installation selected, by stable id, " +
			"with names and descriptions.",
		WhenToUse: "First, to find which repository holds the failing service: match the " +
			"incident's service name against repository names and descriptions, then use " +
			"the id everywhere after.",
		WhenNotToUse: "Not for commit or pull-request content — it returns none. Never " +
			"repeatedly inside one investigation; the selection does not change mid-incident.",
		Arguments:   declared,
		Permissions: "the app installation's own repository grant; nothing beyond it is visible",
		RateLimit:   rateLimitNote,
		Output: "a bounded list of repositories, each with id, name, full name, privacy, " +
			"archive state, default branch and description, plus a truncated flag when " +
			"the installation selected more",
		Run: func(ctx context.Context, request integrations.ToolRequest) (integrations.ToolResult, error) {
			values, err := readArguments(declared, request.Arguments)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			limit, err := values.count("limit", defaultRepositories, maxItemsPerRead)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			token, err := installationTokenFor(ctx, app, request.Integration)
			if err != nil {
				return integrations.ToolResult{}, err
			}

			listed, err := client.Repositories(ctx, token, limit)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			content := make([]repositoryContent, 0, len(listed.Repositories))
			for _, one := range listed.Repositories {
				content = append(content, repositoryContentOf(one))
			}
			return integrations.ToolResult{Content: content, Truncated: listed.Truncated}, nil
		},
	}
}

func readRepositoryTool(app *App, client *Client) integrations.Tool {
	declared := []integrations.ToolArgument{
		{
			Name:        "repositoryId",
			Description: "The repository's stable numeric id from github.list_repositories.",
			Type:        integrations.FieldInteger,
			Required:    true,
		},
	}
	return integrations.Tool{
		Name:       "github.read_repository",
		Capability: ReadRepository,
		Description: "Reads one repository's identity and state by its stable id: name, " +
			"default branch, privacy, archive state, description.",
		WhenToUse: "To confirm what a repository is before reading its history — its " +
			"default branch is what commits and pull requests land on.",
		WhenNotToUse: "Not for history; that is github.read_commits and " +
			"github.read_pull_requests. Not to discover repositories; that is " +
			"github.list_repositories.",
		Arguments:   declared,
		Permissions: "the app installation's own repository grant",
		RateLimit:   rateLimitNote,
		Output:      "one repository with id, name, full name, privacy, archive state, default branch and description",
		Run: func(ctx context.Context, request integrations.ToolRequest) (integrations.ToolResult, error) {
			values, err := readArguments(declared, request.Arguments)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			repository, err := values.identity("repositoryId")
			if err != nil {
				return integrations.ToolResult{}, err
			}
			token, err := installationTokenFor(ctx, app, request.Integration)
			if err != nil {
				return integrations.ToolResult{}, err
			}

			found, err := client.Repository(ctx, token, repository)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			return integrations.ToolResult{Content: repositoryContentOf(found)}, nil
		},
	}
}

func readCommitsTool(app *App, client *Client) integrations.Tool {
	declared := []integrations.ToolArgument{
		{
			Name:        "repositoryId",
			Description: "The repository's stable numeric id from github.list_repositories.",
			Type:        integrations.FieldInteger,
			Required:    true,
		},
		{
			Name: "since",
			Description: "Start of the window, RFC 3339. Use the incident's own window " +
				"widened by an hour or two — the change that caused an incident usually " +
				"landed shortly before it.",
			Type: integrations.FieldString,
		},
		{
			Name:        "until",
			Description: "End of the window, RFC 3339.",
			Type:        integrations.FieldString,
		},
		{
			Name: "limit",
			Description: fmt.Sprintf("How many commits to return, at most %d. Default %d.",
				maxItemsPerRead, defaultCommits),
			Type: integrations.FieldInteger,
		},
	}
	return integrations.Tool{
		Name:       "github.read_commits",
		Capability: ReadCommits,
		Description: "Reads one repository's commits inside a time window, newest first, " +
			"bounded and flagged when the window holds more.",
		WhenToUse: "To answer \"what changed before this broke\": read the incident " +
			"window plus a lead of an hour or two on the repository that owns the failing " +
			"service.",
		WhenNotToUse: "Not for the intent behind a change — that is " +
			"github.read_pull_requests, where review and description live. Not " +
			"unbounded: without a window it reads the recent tail only.",
		Arguments:   declared,
		Permissions: "the app installation's own repository grant",
		RateLimit:   rateLimitNote,
		Output: "a bounded list of commits, each with sha, message, author and authored " +
			"time, plus a truncated flag when the window holds more; an empty repository " +
			"answers an empty list",
		Run: func(ctx context.Context, request integrations.ToolRequest) (integrations.ToolResult, error) {
			values, err := readArguments(declared, request.Arguments)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			repository, err := values.identity("repositoryId")
			if err != nil {
				return integrations.ToolResult{}, err
			}
			limit, err := values.count("limit", defaultCommits, maxItemsPerRead)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			since, err := values.moment("since")
			if err != nil {
				return integrations.ToolResult{}, err
			}
			until, err := values.moment("until")
			if err != nil {
				return integrations.ToolResult{}, err
			}
			token, err := installationTokenFor(ctx, app, request.Integration)
			if err != nil {
				return integrations.ToolResult{}, err
			}

			read, err := client.Commits(ctx, token, CommitsQuery{
				RepositoryID: repository, Since: since, Until: until, Limit: limit,
			})
			if err != nil {
				return integrations.ToolResult{}, err
			}
			content := make([]commitContent, 0, len(read.Commits))
			for _, one := range read.Commits {
				content = append(content, commitContent(one))
			}
			return integrations.ToolResult{Content: content, Truncated: read.Truncated}, nil
		},
	}
}

func readPullRequestsTool(app *App, client *Client) integrations.Tool {
	declared := []integrations.ToolArgument{
		{
			Name:        "repositoryId",
			Description: "The repository's stable numeric id from github.list_repositories.",
			Type:        integrations.FieldInteger,
			Required:    true,
		},
		{
			Name: "limit",
			Description: fmt.Sprintf("How many pull requests to return, at most %d. Default %d.",
				maxItemsPerRead, defaultPullRequests),
			Type: integrations.FieldInteger,
		},
	}
	return integrations.Tool{
		Name:       "github.read_pull_requests",
		Capability: ReadPullRequests,
		Description: "Reads one repository's pull requests, most recently updated first, " +
			"in every state — merged ones are the change context an investigation wants.",
		WhenToUse: "To see what was merged or in flight around the incident: titles, " +
			"branches and merge times say what a bare commit list cannot.",
		WhenNotToUse: "Not for individual commits inside the window; that is " +
			"github.read_commits. Not to find the repository; that is " +
			"github.list_repositories.",
		Arguments:   declared,
		Permissions: "the app installation's own repository grant",
		RateLimit:   rateLimitNote,
		Output: "a bounded list of pull requests, each with number, title, state, merge " +
			"and update times, author and branches, plus a truncated flag when the " +
			"repository holds more",
		Run: func(ctx context.Context, request integrations.ToolRequest) (integrations.ToolResult, error) {
			values, err := readArguments(declared, request.Arguments)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			repository, err := values.identity("repositoryId")
			if err != nil {
				return integrations.ToolResult{}, err
			}
			limit, err := values.count("limit", defaultPullRequests, maxItemsPerRead)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			token, err := installationTokenFor(ctx, app, request.Integration)
			if err != nil {
				return integrations.ToolResult{}, err
			}

			read, err := client.PullRequests(ctx, token, PullsQuery{
				RepositoryID: repository, Limit: limit,
			})
			if err != nil {
				return integrations.ToolResult{}, err
			}
			content := make([]pullRequestContent, 0, len(read.PullRequests))
			for _, one := range read.PullRequests {
				content = append(content, pullRequestContent(one))
			}
			return integrations.ToolResult{Content: content, Truncated: read.Truncated}, nil
		},
	}
}

// installationTokenFor resolves the integration's installation and mints or reuses its
// token. Every tool starts here, so an unconfigured deployment or a broken installation
// id fails the same way everywhere.
func installationTokenFor(
	ctx context.Context, app *App, integration integrations.Integration,
) (string, error) {
	installation, err := installationOf(integration)
	if err != nil {
		return "", err
	}
	return app.installationToken(ctx, installation)
}

func repositoryContentOf(repository Repository) repositoryContent {
	return repositoryContent(repository)
}

// arguments is one call's inputs after the undeclared ones were refused.
type arguments struct {
	values map[string]any
}

// readArguments refuses an argument nothing declares. Dropped arguments are the quiet
// failure mode of tool calling: the caller believes it narrowed the read and it did not.
func readArguments(
	declared []integrations.ToolArgument, given map[string]any,
) (arguments, error) {
	for name := range given {
		if !declaresArgument(declared, name) {
			return arguments{}, fmt.Errorf("argument %q is not one this tool declares", name)
		}
	}
	return arguments{values: given}, nil
}

func declaresArgument(declared []integrations.ToolArgument, name string) bool {
	for _, argument := range declared {
		if argument.Name == name {
			return true
		}
	}
	return false
}

// text reads an optional string argument.
func (a arguments) text(name string) (string, error) {
	value, given := a.values[name]
	if !given {
		return "", nil
	}
	text, isText := value.(string)
	if !isText {
		return "", fmt.Errorf("%s must be text", name)
	}
	return strings.TrimSpace(text), nil
}

// identity reads a required positive whole number, the shape every stable id has.
func (a arguments) identity(name string) (int64, error) {
	value, given := a.values[name]
	if !given {
		return 0, errors.New(name + " is required")
	}
	id, err := wholePositiveID(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a whole positive number", name)
	}
	return id, nil
}

// wholePositiveID is the one reading of a stable id from decoded JSON, shared by
// configuration and tool arguments. Numbers arrive as float64; a whole positive value is
// required, not merely truncated.
func wholePositiveID(value any) (int64, error) {
	number, isNumber := value.(float64)
	if !isNumber || number != float64(int64(number)) || number < 1 {
		return 0, errors.New("not a whole positive number")
	}
	return int64(number), nil
}

// count reads a bounded whole number, applying the default when absent.
func (a arguments) count(name string, fallback, maximum int) (int, error) {
	value, given := a.values[name]
	if !given {
		return fallback, nil
	}
	number, isNumber := value.(float64)
	if !isNumber || number != float64(int64(number)) || number < 1 || int(number) > maximum {
		return 0, fmt.Errorf("%s must be a whole number between 1 and %d", name, maximum)
	}
	return int(number), nil
}

// moment reads an optional RFC 3339 timestamp.
func (a arguments) moment(name string) (time.Time, error) {
	text, err := a.text(name)
	if err != nil || text == "" {
		return time.Time{}, err
	}
	at, parseErr := time.Parse(time.RFC3339, text)
	if parseErr != nil {
		return time.Time{}, fmt.Errorf("%s must be an RFC 3339 time such as "+
			"2026-01-02T15:04:05Z", name)
	}
	return at, nil
}
