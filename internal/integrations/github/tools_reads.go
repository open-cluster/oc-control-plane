package github

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// The five deep reads and the release listing: the causal workflow past the commit
// message. Each is one bounded step — the actual diff, the PR's intent and CI health,
// the pipeline's runs, a failing job's log tail, the configuration a change touched,
// and what shipped.

// The named bounds on the deep reads.
const (
	// maxFilesRendered bounds how many changed files one answer carries; the vendor's
	// own page holds at most 300 for a commit and 3000 ever for a pull request.
	maxFilesRendered = 100
	// maxPatchBytes bounds one file's rendered patch. Cut at a line boundary with the
	// cut named, so a diff reads as a diff.
	maxPatchBytes = 2048
	// maxPatchBudget bounds all patches in one answer together, so a hundred-file
	// commit answers file names and counts rather than a novel.
	maxPatchBudget = 24 << 10
	// logTailBytes is the slice of a failing job's log one read answers: the tail,
	// where the failure is.
	logTailBytes = 16 << 10
	// maxFileToolBytes bounds one file read.
	maxFileToolBytes = 64 << 10
	maxChecks        = 50
	defaultRuns      = 20
	maxRunsPerRead   = 50
	defaultReleases  = 20
	maxReleases      = 50
)

// changedFileContent is one changed file as a tool reports it.
type changedFileContent struct {
	Path      string `json:"path"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Patch     string `json:"patch,omitempty"`
	// PatchTruncated says the patch was cut at the bound; PatchOmitted says the
	// answer's patch budget was spent before this file.
	PatchTruncated bool `json:"patchTruncated,omitempty"`
	PatchOmitted   bool `json:"patchOmitted,omitempty"`
}

// renderChangedFiles bounds the files and their patches inside the answer's budget.
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

// cutAtLine cuts text inside the bound at the last whole line, so a diff still reads
// as a diff.
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
		Name:       "github.read_commit",
		Capability: ReadCommit,
		Description: "Reads one commit whole: its changed files and their patches, " +
			"bounded — the change itself, not the message about it.",
		WhenToUse: "When github.read_commits surfaced a suspicious commit and \"what " +
			"did it actually change\" is the question: which files, which lines, " +
			"whether the change plausibly causes the production behavior.",
		WhenNotToUse: "Not for listing history; that is github.read_commits. Not on " +
			"every commit in the window — read the ones whose message or timing makes " +
			"them suspects.",
		Arguments:   declared,
		Permissions: "the app installation needs the Contents read permission",
		Output: "the commit's sha, message, author, permalink and changed files, each " +
			"with path, status, line counts and a bounded patch; flags say when files " +
			"or patches were cut, and a very large diff is refused by the vendor with " +
			"the reason",
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
		Name:       "github.read_pull_request",
		Capability: ReadPullRequest,
		Description: "Reads one pull request whole: its description, changed files and " +
			"CI check status — the intent and health of a change as one answer.",
		WhenToUse: "When a commit references a pull request and \"why was this change " +
			"made, and did CI object\" is the question. The description carries the " +
			"intent a commit message abbreviates.",
		WhenNotToUse: "Not to find the number — commit messages from " +
			"github.read_commits carry it. Not for the raw diff of one commit; that " +
			"is github.read_commit.",
		Arguments:   declared,
		Permissions: "the app installation needs the Contents and Checks read permissions",
		Output: "the pull request's title, description, state, merge time, author, " +
			"branches and permalink; its changed files with bounded patches; and its " +
			"CI check runs with status and conclusion — absent with the reason when " +
			"the installation cannot read checks",
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
			// Checks are best-effort: an installation without the Checks permission
			// still answers the pull request, with the absence named rather than the
			// whole read failing.
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
		Name:       "github.read_workflow_runs",
		Capability: ReadWorkflowRuns,
		Description: "Reads a repository's CI/CD workflow runs inside a time window, " +
			"newest first, with status and conclusion.",
		WhenToUse: "To answer \"did the deploy pipeline object\": failed or cancelled " +
			"runs near the incident are leads, and a run's id is what " +
			"github.read_job_log takes.",
		WhenNotToUse: "Not for the code change itself; that is github.read_commit. " +
			"Not unbounded — the window is the incident's own.",
		Arguments:   declared,
		Permissions: "the app installation needs the Actions read permission",
		Output: "a bounded list of runs, each with id, workflow name, branch, head " +
			"sha, trigger event, status, conclusion, start time and permalink, plus a " +
			"truncated flag when the window holds more",
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
				Content:   runs,
				Truncated: read.Truncated,
				Summary:   fmt.Sprintf("%d workflow runs in the window", len(runs)),
				Sources:   []string{strconv.FormatInt(repository, 10)},
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
		Name:       "github.read_job_log",
		Capability: ReadJobLog,
		Description: "Reads the log tail of a workflow run's failing job — the end of " +
			"the log, where the failure speaks.",
		WhenToUse: "When github.read_workflow_runs showed a failed run and \"what " +
			"exactly failed\" is the question: the failing step and its final output.",
		WhenNotToUse: "Not on succeeded runs — their logs rarely answer anything. Not " +
			"for the change that broke CI; that is github.read_commit.",
		Arguments:   declared,
		Permissions: "the app installation needs the Actions read permission",
		Output: fmt.Sprintf("the run's jobs with status, conclusion and failed step, "+
			"and the chosen job's last %d bytes of log; the truncated flag says the "+
			"log held more", logTailBytes),
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

// chooseJob resolves which job's log to read: the named one, or the run's first
// failed one — the job an investigation is here for.
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
		Name:       "github.read_file",
		Capability: ReadFile,
		Description: "Reads one file's contents at a ref, bounded — the configuration " +
			"a commit touched can be inspected, not guessed.",
		WhenToUse: "When a changed file's full context matters: the config value " +
			"around a diff hunk, the threshold a commit moved, the manifest a deploy " +
			"reads.",
		WhenNotToUse: "Not to browse a repository — read files a commit or diff named. " +
			"Not for the change itself; that is github.read_commit.",
		Arguments:   declared,
		Permissions: "the app installation needs the Contents read permission",
		Output: fmt.Sprintf("the file's first %d bytes at the ref, with a truncated "+
			"flag when it holds more", maxFileToolBytes),
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
		Name:       "github.list_releases",
		Capability: ListReleases,
		Description: "Lists a repository's releases, newest first — what shipped, " +
			"and when.",
		WhenToUse: "To answer \"what shipped near the incident window\": a release " +
			"published just before the alert is the first suspect.",
		WhenNotToUse: "Not for the commits inside a release; that is " +
			"github.read_commits with the window. Bare un-released tags are not " +
			"listed, by the vendor's own rule.",
		Arguments:   declared,
		Permissions: "the app installation needs the Contents read permission",
		Output: "a bounded list of releases, each with name, tag, publish time, " +
			"author, prerelease flag and permalink, plus a truncated flag when the " +
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

// firstLine keeps a wrapped refusal to its first sentence for an embedded note.
func firstLine(text string) string {
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		return text[:index]
	}
	return text
}
