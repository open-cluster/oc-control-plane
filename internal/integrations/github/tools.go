package github

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// The three read-only tools. Repositories are addressed by their stable numeric ids —
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

// tools is the declared set, one-to-one with the capabilities the definition declares.
func tools(app *App, client *Client) []integrations.Tool {
	return []integrations.Tool{
		listRepositoriesTool(app, client),
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
		Output: "a bounded list of repositories, each with id, name, full name, privacy, " +
			"archive state, default branch and description, plus a truncated flag when " +
			"the installation selected more",
		Run: func(ctx context.Context, request integrations.ToolRequest) (integrations.ToolResult, error) {
			values, err := integrations.ReadArguments(declared, request.Arguments)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			limit, err := values.Count("limit", defaultRepositories, maxItemsPerRead)
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
			sources := make([]string, 0, len(listed.Repositories))
			for _, one := range listed.Repositories {
				content = append(content, repositoryContentOf(one))
				sources = append(sources, strconv.FormatInt(one.ID, 10))
			}
			return integrations.ToolResult{
				Content:   content,
				Truncated: listed.Truncated,
				Summary:   fmt.Sprintf("%d repositories granted", len(content)),
				Sources:   sources,
			}, nil
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
			Description: "Start of the window, RFC 3339. The investigation's window " +
				"already reaches back before the incident began, and every read is " +
				"clamped inside it — a wider ask does not widen the read.",
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
		WhenToUse: "To answer \"what changed before this broke\": read the incident's " +
			"own window on the repository that owns the failing service.",
		WhenNotToUse: "Not for what was merged as a unit — that is " +
			"github.read_pull_requests, which carries titles, branches and merge times. " +
			"Not unbounded: without a window it reads the recent tail only.",
		Arguments:   declared,
		Permissions: "the app installation's own repository grant",
		Output: "a bounded list of commits, each with sha, message, author and authored " +
			"time, plus a truncated flag when the window holds more; an empty repository " +
			"answers an empty list",
		Run: func(ctx context.Context, request integrations.ToolRequest) (integrations.ToolResult, error) {
			values, err := integrations.ReadArguments(declared, request.Arguments)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			repository, err := values.Identity("repositoryId")
			if err != nil {
				return integrations.ToolResult{}, err
			}
			limit, err := values.Count("limit", defaultCommits, maxItemsPerRead)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			since, err := values.Moment("since")
			if err != nil {
				return integrations.ToolResult{}, err
			}
			until, err := values.Moment("until")
			if err != nil {
				return integrations.ToolResult{}, err
			}
			since, until = request.ClampWindow(since, until)
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
			return integrations.ToolResult{
				Content:   content,
				Truncated: read.Truncated,
				Summary:   fmt.Sprintf("%d commits in the window", len(content)),
				Sources:   []string{strconv.FormatInt(repository, 10)},
			}, nil
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
			"branches and merge times say what a bare commit list cannot. The output " +
			"carries no description or review content.",
		WhenNotToUse: "Not for individual commits inside the window; that is " +
			"github.read_commits. Not to find the repository; that is " +
			"github.list_repositories.",
		Arguments:   declared,
		Permissions: "the app installation's own repository grant",
		Output: "a bounded list of pull requests, each with number, title, state, merge " +
			"and update times, author and branches, plus a truncated flag when the " +
			"repository holds more",
		Run: func(ctx context.Context, request integrations.ToolRequest) (integrations.ToolResult, error) {
			values, err := integrations.ReadArguments(declared, request.Arguments)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			repository, err := values.Identity("repositoryId")
			if err != nil {
				return integrations.ToolResult{}, err
			}
			limit, err := values.Count("limit", defaultPullRequests, maxItemsPerRead)
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
			return integrations.ToolResult{
				Content:   content,
				Truncated: read.Truncated,
				Summary:   fmt.Sprintf("%d pull requests, newest first", len(content)),
				Sources:   []string{strconv.FormatInt(repository, 10)},
			}, nil
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

// wholePositiveID is the one reading of a stable id from decoded JSON configuration.
// Numbers arrive as float64; a whole positive value is required, not merely truncated.
// Tool arguments read theirs through the shared integrations.Arguments instead.
func wholePositiveID(value any) (int64, error) {
	number, isNumber := value.(float64)
	if !isNumber || number != float64(int64(number)) || number < 1 {
		return 0, errors.New("not a whole positive number")
	}
	return int64(number), nil
}
