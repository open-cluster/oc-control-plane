package github

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

const (
	maxItemsPerRead     = 100
	defaultRepositories = 50
	defaultCommits      = 30
	maxRepositoryPages  = 5
)

// tools is the declared set of bounded GitHub reads:
// the eight steps of the causal workflow, from finding the repository to reading what shipped.
func tools(app *App, client *Client) []integrations.Tool {
	return []integrations.Tool{
		listRepositoriesTool(app, client),
		readCommitsTool(app, client),
		readCommitTool(app, client),
		readPullRequestTool(app, client),
		readWorkflowRunsTool(app, client),
		readJobLogTool(app, client),
		readFileTool(app, client),
		listReleasesTool(app, client),
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
	SHA       string `json:"sha"`
	Message   string `json:"message"`
	Author    string `json:"author,omitempty"`
	AuthorAt  string `json:"authorAt,omitempty"`
	Permalink string `json:"permalink,omitempty"`
}

func listRepositoriesTool(app *App, client *Client) integrations.Tool {
	declared := []integrations.ToolArgument{
		{
			Name: "nameContains",
			Description: "Case-insensitive text to select repositories by name or " +
				"description. Use the incident's service name.",
			Type: integrations.FieldString,
		},
		{
			Name: "limit",
			Description: fmt.Sprintf("How many repositories to return, at most %d. Default %d.",
				maxItemsPerRead, defaultRepositories),
			Type: integrations.FieldInteger,
		},
	}
	return integrations.Tool{
		Name: "github.list_repositories",
		Description: "Lists the repositories this installation selected, by stable id, " +
			"with names and descriptions.",
		WhenToUse: "First, to find which repository holds the failing service: filter by " +
			"the incident's service name, then use the id everywhere after.",
		WhenNotToUse: "Not for commit or pull-request content — it returns none. Never " +
			"repeatedly inside one investigation; the selection does not change mid-incident.",
		Arguments:   declared,
		Permissions: permissionProse("github.list_repositories"),
		Output: "a bounded list of repositories, each with id, name, full name, privacy, " +
			"archive state, default branch and description, plus a truncated flag when " +
			"more matched than were returned or the walk stopped early",
		Run: func(ctx context.Context, request integrations.ToolRequest) (integrations.ToolResult, error) {
			values, err := integrations.ReadArguments(declared, request.Arguments)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			limit, err := values.Count("limit", defaultRepositories, maxItemsPerRead)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			needle, err := values.Text("nameContains")
			if err != nil {
				return integrations.ToolResult{}, err
			}
			token, err := installationTokenFor(ctx, app, request.Integration)
			if err != nil {
				return integrations.ToolResult{}, err
			}

			// The filter runs inside a bounded pagination walk, so a matching
			// repository beyond page one is still found.
			var content []repositoryContent
			var sources []string
			matched, walkEnded := 0, false
			for page := 1; page <= maxRepositoryPages; page++ {
				listed, err := client.Repositories(ctx, token, maxItemsPerRead, page)
				if err != nil {
					return integrations.ToolResult{}, err
				}
				for _, one := range listed.Repositories {
					if !matchesRepository(one, needle) {
						continue
					}
					matched++
					if len(content) < limit {
						content = append(content, repositoryContentOf(one))
						sources = append(sources, strconv.FormatInt(one.ID, 10))
					}
				}
				if !listed.NextPage {
					walkEnded = true
					break
				}
				if len(content) >= limit {
					break
				}
			}
			// A whole walk answers untruncated however many pages it took: the
			// per-page Truncated says one PAGE held less than the installation, which
			// is every page of a multi-page grant and not what this answer means.
			return integrations.ToolResult{
				Content:   content,
				Truncated: matched > len(content) || !walkEnded,
				Summary:   fmt.Sprintf("%d repositories matched", len(content)),
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
		Name: "github.read_commits",
		Description: "Reads one repository's commits inside a time window, newest first, " +
			"bounded and flagged when the window holds more.",
		WhenToUse: "To answer \"what changed before this broke\": read the incident's " +
			"own window on the repository that owns the failing service.",
		WhenNotToUse: "Not for a commit's actual diff — that is github.read_commit. " +
			"Omitting the window does NOT widen the read to the repository's recent " +
			"history: every read is clamped into the investigation's own window, which " +
			"may be short, and the result states the window it actually covered.",
		Arguments:   declared,
		Permissions: permissionProse("github.read_commits"),
		Output: "a bounded list of commits, each with sha, message, author, authored " +
			"time and permalink, plus a truncated flag when the window holds more; an " +
			"empty repository answers an empty list; a message like \"Merge pull " +
			"request #123\" carries the number github.read_pull_request takes",
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
				content = append(content, commitContent{
					SHA: one.SHA, Message: one.Message, Author: one.Author,
					AuthorAt: one.AuthorAt, Permalink: one.HTMLURL,
				})
			}
			return integrations.ToolResult{
				Content:    content,
				Truncated:  read.Truncated,
				Summary:    fmt.Sprintf("%d commits in the window", len(content)),
				Sources:    []string{strconv.FormatInt(repository, 10)},
				WindowFrom: since, WindowUntil: until,
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

// matchesRepository reports whether a repository's names or description carry the
// needle. An empty needle selects everything, which is the unfiltered listing.
func matchesRepository(repository Repository, needle string) bool {
	if needle == "" {
		return true
	}
	needle = strings.ToLower(needle)
	return strings.Contains(strings.ToLower(repository.Name), needle) ||
		strings.Contains(strings.ToLower(repository.FullName), needle) ||
		strings.Contains(strings.ToLower(repository.Description), needle)
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
