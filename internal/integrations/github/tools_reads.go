package github

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

const (
	maxFilesRendered = 100
	maxPatchBytes    = 2048
	maxPatchBudget   = 24 << 10
	logTailBytes     = 16 << 10
	maxFileToolBytes = 64 << 10
	maxChecks        = 50
	defaultRuns      = 20
	maxRunsPerRead   = 50
	defaultReleases  = 20
	maxReleases      = 50
)

type changedFileContent struct {
	Path           string `json:"path"`
	Status         string `json:"status"`
	Additions      int    `json:"additions"`
	Deletions      int    `json:"deletions"`
	Patch          string `json:"patch,omitempty"`
	PatchTruncated bool   `json:"patchTruncated,omitempty"`
	PatchOmitted   bool   `json:"patchOmitted,omitempty"`
}

func renderChangedFiles(files []ChangedFile) ([]changedFileContent, bool) {
	truncated := len(files) > maxFilesRendered
	if truncated {
		files = files[:maxFilesRendered]
	}
	budget := maxPatchBudget
	rendered := make([]changedFileContent, 0, len(files))
	for _, one := range files {
		content := changedFileContent{
			Path: one.Path, Status: one.Status,
			Additions: one.Additions, Deletions: one.Deletions,
		}
		switch {
		case one.Patch == "":
		case budget <= 0:
			content.PatchOmitted = true
		default:
			patch := one.Patch
			bound := min(maxPatchBytes, budget)
			if len(patch) > bound {
				patch = cutAtLine(patch, bound)
				content.PatchTruncated = true
			}
			content.Patch = patch
			budget -= len(patch)
		}
		rendered = append(rendered, content)
	}
	return rendered, truncated
}

func cutAtLine(text string, bound int) string {
	cut := text[:bound]
	if last := strings.LastIndexByte(cut, '\n'); last > 0 {
		cut = cut[:last]
	}
	return cut + "\n… [patch cut at " + strconv.Itoa(bound) + " bytes]"
}

func readCommitTool(app *App, client *Client) integrations.Tool {
	declared := []integrations.ToolArgument{
		{
			Name:        "repositoryId",
			Description: "The repository's stable numeric id from github.list_repositories.",
			Type:        integrations.FieldInteger,
			Required:    true,
		},
		{
			Name:        "sha",
			Description: "The commit sha from github.read_commits.",
			Type:        integrations.FieldString,
			Required:    true,
		},
	}
	return integrations.Tool{
		Name: toolReadCommit,
		Description: "Read one commit's metadata, changed files, and bounded patches. Use " +
			"after github.read_commits identifies a relevant commit and the Investigation " +
			"needs the actual change. Do not use to list history or prove deployment. " +
			"Results report omitted files and truncated patches.",
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
			sha, err := values.Required("sha")
			if err != nil {
				return integrations.ToolResult{}, err
			}
			token, err := installationTokenFor(ctx, app, request.Integration)
			if err != nil {
				return integrations.ToolResult{}, err
			}

			detail, err := client.Commit(ctx, token, repository, sha)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			files, cut := renderChangedFiles(detail.Files)
			return integrations.ToolResult{
				Content: map[string]any{
					"sha": detail.SHA, "message": detail.Message,
					"author": detail.Author, "authorAt": detail.AuthorAt,
					"permalink": detail.HTMLURL, "files": files,
				},
				Truncated: cut || detail.FilesTruncated,
				Summary: fmt.Sprintf("commit %.12s: %d files changed",
					detail.SHA, len(detail.Files)),
				Sources: []string{strconv.FormatInt(repository, 10) + "@" + detail.SHA},
			}, nil
		},
	}
}

func readPullRequestTool(app *App, client *Client) integrations.Tool {
	declared := []integrations.ToolArgument{
		{
			Name:        "repositoryId",
			Description: "The repository's stable numeric id from github.list_repositories.",
			Type:        integrations.FieldInteger,
			Required:    true,
		},
		{
			Name: "number",
			Description: "The pull request number, usually from a commit message such " +
				"as \"Merge pull request #123\" or \"Title (#123)\".",
			Type:     integrations.FieldInteger,
			Required: true,
		},
	}
	return integrations.Tool{
		Name: toolReadPullRequest,
		Description: "Read one pull request's intent, merge state, changed files, bounded " +
			"patches, and available CI checks. Use when a known pull request explains why a " +
			"change was made or whether CI objected. Do not use to discover its number or " +
			"prove production rollout; start from a commit that names it.",
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
			number, err := values.Identity("number")
			if err != nil {
				return integrations.ToolResult{}, err
			}
			token, err := installationTokenFor(ctx, app, request.Integration)
			if err != nil {
				return integrations.ToolResult{}, err
			}

			detail, err := client.PullRequest(ctx, token, repository, int(number))
			if err != nil {
				return integrations.ToolResult{}, err
			}
			files, filesTruncated, err := client.PullRequestFiles(
				ctx, token, repository, int(number), maxFilesRendered)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			rendered, cut := renderChangedFiles(files)

			content := map[string]any{
				"number": detail.Number, "title": detail.Title,
				"description": detail.Body, "state": detail.State,
				"merged": detail.Merged, "mergedAt": detail.MergedAt,
				"author": detail.Author, "head": detail.Head, "base": detail.Base,
				"permalink": detail.HTMLURL, "files": rendered,
			}
			checks, checksTruncated, checksErr := client.CheckRuns(
				ctx, token, repository, detail.HeadSHA, maxChecks)
			if checksErr != nil {
				content["checksUnavailable"] = "the check runs could not be read: " +
					firstLine(checksErr.Error())
			} else {
				summaries := make([]map[string]string, 0, len(checks))
				for _, check := range checks {
					summaries = append(summaries, map[string]string{
						"name": check.Name, "status": check.Status,
						"conclusion": check.Conclusion,
					})
				}
				content["checks"] = summaries
				if checksTruncated {
					content["checksTruncated"] = "true"
				}
			}

			return integrations.ToolResult{
				Content:   content,
				Truncated: cut || filesTruncated,
				Summary: fmt.Sprintf("pull request #%d (%s): %d files",
					detail.Number, detail.State, len(files)),
				Sources: []string{strconv.FormatInt(repository, 10) +
					"#" + strconv.FormatInt(number, 10)},
			}, nil
		},
	}
}

func readWorkflowRunsTool(app *App, client *Client) integrations.Tool {
	declared := []integrations.ToolArgument{
		{
			Name:        "repositoryId",
			Description: "The repository's stable numeric id from github.list_repositories.",
			Type:        integrations.FieldInteger,
			Required:    true,
		},
		{
			Name: "since",
			Description: "Start of the window, RFC 3339. Every read is clamped inside " +
				"the investigation's window.",
			Type: integrations.FieldString,
		},
		{
			Name:        "until",
			Description: "End of the window, RFC 3339.",
			Type:        integrations.FieldString,
		},
		{
			Name: "limit",
			Description: fmt.Sprintf("How many runs to return, at most %d. Default %d.",
				maxRunsPerRead, defaultRuns),
			Type: integrations.FieldInteger,
		},
	}
	return integrations.Tool{
		Name: toolReadWorkflowRuns,
		Description: "Read CI/CD workflow runs for a repository in the Investigation " +
			"window, newest first. Use to find failed or cancelled runs and obtain a run ID " +
			"for github.read_job_log. Do not use a successful run to prove production health " +
			"or deployment. Results are bounded and report truncation.",
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
			limit, err := values.Count("limit", defaultRuns, maxRunsPerRead)
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

			read, err := client.WorkflowRuns(ctx, token, RunsQuery{
				RepositoryID: repository, Since: since, Until: until, Limit: limit,
			})
			if err != nil {
				return integrations.ToolResult{}, err
			}
			runs := make([]map[string]any, 0, len(read.Runs))
			for _, one := range read.Runs {
				runs = append(runs, map[string]any{
					"id": one.ID, "name": one.Name, "branch": one.Branch,
					"headSha": one.HeadSHA, "event": one.Event, "status": one.Status,
					"conclusion": one.Conclusion, "createdAt": one.CreatedAt,
					"permalink": one.HTMLURL,
				})
			}
			return integrations.ToolResult{
				Content:    runs,
				Truncated:  read.Truncated,
				Summary:    fmt.Sprintf("%d workflow runs in the window", len(runs)),
				Sources:    []string{strconv.FormatInt(repository, 10)},
				WindowFrom: since, WindowUntil: until,
			}, nil
		},
	}
}

func readJobLogTool(app *App, client *Client) integrations.Tool {
	declared := []integrations.ToolArgument{
		{
			Name:        "repositoryId",
			Description: "The repository's stable numeric id from github.list_repositories.",
			Type:        integrations.FieldInteger,
			Required:    true,
		},
		{
			Name:        "runId",
			Description: "The workflow run id from github.read_workflow_runs.",
			Type:        integrations.FieldInteger,
			Required:    true,
		},
		{
			Name: "jobId",
			Description: "One job's id, to read that job instead of the run's first " +
				"failed one.",
			Type: integrations.FieldInteger,
		},
	}
	return integrations.Tool{
		Name: toolReadJobLog,
		Description: fmt.Sprintf("Read the failing job and final %d bytes of its log from "+
			"a known workflow run. Use to identify the failed step and its final output. Do "+
			"not use for successful runs or to identify the code change. Results report log "+
			"truncation.", logTailBytes),
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
			run, err := values.Identity("runId")
			if err != nil {
				return integrations.ToolResult{}, err
			}
			token, err := installationTokenFor(ctx, app, request.Integration)
			if err != nil {
				return integrations.ToolResult{}, err
			}

			jobs, err := client.RunJobs(ctx, token, repository, run, maxChecks)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			chosen, err := chooseJob(values, jobs)
			if err != nil {
				return integrations.ToolResult{}, err
			}

			log, truncated, err := client.JobLog(ctx, token, repository, chosen.ID, logTailBytes)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			summaries := make([]map[string]any, 0, len(jobs))
			for _, job := range jobs {
				summaries = append(summaries, map[string]any{
					"id": job.ID, "name": job.Name, "status": job.Status,
					"conclusion": job.Conclusion, "failedStep": job.FailedStep,
				})
			}
			return integrations.ToolResult{
				Content: map[string]any{
					"jobs": summaries, "job": chosen.Name,
					"failedStep": chosen.FailedStep, "logTail": log,
				},
				Truncated: truncated,
				Summary: fmt.Sprintf("job %q (%s): log tail of %d bytes",
					chosen.Name, chosen.Conclusion, len(log)),
				Sources: []string{strconv.FormatInt(repository, 10) +
					"/runs/" + strconv.FormatInt(run, 10)},
			}, nil
		},
	}
}

func chooseJob(values integrations.Arguments, jobs []Job) (Job, error) {
	id, named, err := values.OptionalIdentity("jobId")
	if err != nil {
		return Job{}, err
	}
	if named {
		for _, job := range jobs {
			if job.ID == id {
				return job, nil
			}
		}
		return Job{}, fmt.Errorf("job %d is not one of this run's jobs", id)
	}
	for _, job := range jobs {
		if job.Conclusion == "failure" {
			return job, nil
		}
	}
	if len(jobs) == 0 {
		return Job{}, fmt.Errorf("the run has no jobs to read")
	}
	return jobs[0], nil
}

func readFileTool(app *App, client *Client) integrations.Tool {
	declared := []integrations.ToolArgument{
		{
			Name:        "repositoryId",
			Description: "The repository's stable numeric id from github.list_repositories.",
			Type:        integrations.FieldInteger,
			Required:    true,
		},
		{
			Name:        "path",
			Description: "The file's path in the repository, such as \"config/app.yaml\".",
			Type:        integrations.FieldString,
			Required:    true,
		},
		{
			Name: "ref",
			Description: "The branch, tag or commit sha to read at. Default: the " +
				"repository's default branch.",
			Type: integrations.FieldString,
		},
	}
	return integrations.Tool{
		Name: toolReadFile,
		Description: fmt.Sprintf("Read the first %d bytes of one known file at a branch, "+
			"tag, or commit. Use when a changed file's surrounding configuration matters. "+
			"Do not browse with guessed paths or use this instead of github.read_commit for "+
			"the change itself. Results report truncation.", maxFileToolBytes),
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
			path, err := values.Required("path")
			if err != nil {
				return integrations.ToolResult{}, err
			}
			ref, err := values.Text("ref")
			if err != nil {
				return integrations.ToolResult{}, err
			}
			token, err := installationTokenFor(ctx, app, request.Integration)
			if err != nil {
				return integrations.ToolResult{}, err
			}

			read, err := client.File(ctx, token, repository, path, ref, maxFileToolBytes)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			at := ref
			if at == "" {
				at = "the default branch"
			}
			return integrations.ToolResult{
				Content: map[string]any{
					"path": read.Path, "ref": read.Ref, "content": read.Content,
				},
				Truncated: read.Truncated,
				Summary:   fmt.Sprintf("%s at %s: %d bytes", path, at, len(read.Content)),
				Sources:   []string{strconv.FormatInt(repository, 10) + ":" + path},
			}, nil
		},
	}
}

func listReleasesTool(app *App, client *Client) integrations.Tool {
	declared := []integrations.ToolArgument{
		{
			Name:        "repositoryId",
			Description: "The repository's stable numeric id from github.list_repositories.",
			Type:        integrations.FieldInteger,
			Required:    true,
		},
		{
			Name: "limit",
			Description: fmt.Sprintf("How many releases to return, at most %d. Default %d.",
				maxReleases, defaultReleases),
			Type: integrations.FieldInteger,
		},
	}
	return integrations.Tool{
		Name: toolListReleases,
		Description: "List a repository's published releases, newest first, with tags, " +
			"publish times, authors, prerelease state, and links. Use to identify artifacts " +
			"published near an incident. Do not treat a release as proof of production " +
			"rollout or use it to inspect commits. Results are bounded and report truncation.",
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
			limit, err := values.Count("limit", defaultReleases, maxReleases)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			token, err := installationTokenFor(ctx, app, request.Integration)
			if err != nil {
				return integrations.ToolResult{}, err
			}

			read, err := client.Releases(ctx, token, repository, limit)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			releases := make([]map[string]any, 0, len(read.Releases))
			for _, one := range read.Releases {
				releases = append(releases, map[string]any{
					"name": one.Name, "tag": one.Tag, "publishedAt": one.PublishedAt,
					"author": one.Author, "prerelease": one.Prerelease,
					"permalink": one.HTMLURL,
				})
			}
			return integrations.ToolResult{
				Content:   releases,
				Truncated: read.Truncated,
				Summary:   fmt.Sprintf("%d releases, newest first", len(releases)),
				Sources:   []string{strconv.FormatInt(repository, 10)},
			}, nil
		},
	}
}

func firstLine(text string) string {
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		return text[:index]
	}
	return text
}
