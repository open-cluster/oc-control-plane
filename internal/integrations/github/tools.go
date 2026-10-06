package github

import (
	"context"
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

type repositoryContent struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	FullName      string `json:"fullName"`
	Private       bool   `json:"private"`
	Archived      bool   `json:"archived"`
	DefaultBranch string `json:"defaultBranch,omitempty"`
	Description   string `json:"description,omitempty"`
}

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
		Name: toolListRepositories,
		Description: "List repositories accessible to this Integration, with stable IDs, " +
			"names, descriptions, visibility, archive state, and default branches. Use to " +
			"find the repository for an affected service before repository-specific reads. " +
			"Do not use for commits or pull requests. Results are bounded and report truncation.",
		Arguments: declared,
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
		Name: toolReadCommits,
		Description: "Read commits from an accessible repository in the Investigation " +
			"window. Use when checking whether a code change preceded the incident. Do not " +
			"use to prove deployment; a commit alone does not establish production rollout. " +
			"Results are newest first, bounded, and report truncation.",
		Arguments: declared,
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

func matchesRepository(repository Repository, needle string) bool {
	if needle == "" {
		return true
	}
	needle = strings.ToLower(needle)
	return strings.Contains(strings.ToLower(repository.Name), needle) ||
		strings.Contains(strings.ToLower(repository.FullName), needle) ||
		strings.Contains(strings.ToLower(repository.Description), needle)
}
