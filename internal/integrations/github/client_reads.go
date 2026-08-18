package github

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// The causal-workflow reads: one commit's actual diff, one pull request's intent and
// health, the CI runs and a failing job's log, a file at a ref, and what shipped.
// Known provider facts are encoded in behavior here, never left as prose: commit file
// listings paginate past 300 entries and may 5xx on oversized diffs, pull-request file
// listings cap at 3000, contents over 1 MB are raw-only, releases exclude bare tags,
// and the log download 302s to a URL that expires in about a minute — which is why the
// log is fetched in one bounded read, tail first.

// maxLargeResponseBytes bounds the two reads documented to outgrow the standard bound:
// commit-with-patch and log downloads.
const maxLargeResponseBytes = 16 << 20

// commitFilePageLimit is the vendor's own page size for a commit's file listing: files
// past this many are served on later pages, which this read reports as truncation
// rather than walking — a 300-file commit's tail is not where an investigation starts.
const commitFilePageLimit = 300

// ChangedFile is one file a commit or pull request touched.
type ChangedFile struct {
	Path      string
	Status    string
	Additions int
	Deletions int
	// Patch is the unified diff hunk, empty for binary and very large files where the
	// vendor omits it; the tool bounds it further before it reaches a prompt.
	Patch string
}

// CommitDetail is one commit whole: the change itself, not the message about it.
type CommitDetail struct {
	SHA      string
	Message  string
	Author   string
	AuthorAt string
	HTMLURL  string
	Files    []ChangedFile
	// FilesTruncated reports the commit touched more files than one page carries.
	FilesTruncated bool
}

// Commit reads one commit with its changed files and patches. The vendor may refuse an
// oversized diff with a 5xx, which is answered as what it is rather than an outage.
func (c *Client) Commit(
	ctx context.Context, token string, repository int64, sha string,
) (CommitDetail, error) {
	var decoded struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message string `json:"message"`
			Author  struct {
				Name string `json:"name"`
				Date string `json:"date"`
			} `json:"author"`
		} `json:"commit"`
		Author *struct {
			Login string `json:"login"`
		} `json:"author"`
		HTMLURL string     `json:"html_url"`
		Files   []fileJSON `json:"files"`
	}
	_, err := c.repoReadBounded(ctx, token, repository, "/commits/"+url.PathEscape(sha),
		nil, maxLargeResponseBytes, &decoded)
	var refusal *APIError
	if errors.As(err, &refusal) && refusal.Status >= 500 {
		return CommitDetail{}, fmt.Errorf(
			"github could not render this commit's diff (%d: %s); very large diffs are "+
				"refused by the vendor — read the pull request or individual files instead",
			refusal.Status, refusal.Message)
	}
	if err != nil {
		return CommitDetail{}, err
	}

	detail := CommitDetail{
		SHA:            decoded.SHA,
		Message:        decoded.Commit.Message,
		Author:         decoded.Commit.Author.Name,
		AuthorAt:       decoded.Commit.Author.Date,
		HTMLURL:        decoded.HTMLURL,
		Files:          changedFiles(decoded.Files),
		FilesTruncated: len(decoded.Files) >= commitFilePageLimit,
	}
	if decoded.Author != nil && decoded.Author.Login != "" {
		detail.Author = decoded.Author.Login
	}
	return detail, nil
}

// PullRequestDetail is one pull request whole: its intent (the body), its state, and
// where it went.
type PullRequestDetail struct {
	Number    int
	Title     string
	Body      string
	State     string
	Merged    bool
	MergedAt  string
	UpdatedAt string
	Author    string
	Head      string
	HeadSHA   string
	Base      string
	HTMLURL   string
}

// PullRequest reads one pull request's detail.
func (c *Client) PullRequest(
	ctx context.Context, token string, repository int64, number int,
) (PullRequestDetail, error) {
	var decoded struct {
		Number    int     `json:"number"`
		Title     string  `json:"title"`
		Body      string  `json:"body"`
		State     string  `json:"state"`
		Merged    bool    `json:"merged"`
		MergedAt  *string `json:"merged_at"`
		UpdatedAt string  `json:"updated_at"`
		User      *struct {
			Login string `json:"login"`
		} `json:"user"`
		Head struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
		HTMLURL string `json:"html_url"`
	}
	_, err := c.repoRead(ctx, token, repository, "/pulls/"+strconv.Itoa(number), nil, &decoded)
	if err != nil {
		return PullRequestDetail{}, err
	}
	detail := PullRequestDetail{
		Number: decoded.Number, Title: decoded.Title, Body: decoded.Body,
		State: decoded.State, Merged: decoded.Merged, UpdatedAt: decoded.UpdatedAt,
		Head: decoded.Head.Ref, HeadSHA: decoded.Head.SHA, Base: decoded.Base.Ref,
		HTMLURL: decoded.HTMLURL,
	}
	if decoded.MergedAt != nil {
		detail.MergedAt = *decoded.MergedAt
	}
	if decoded.User != nil {
		detail.Author = decoded.User.Login
	}
	return detail, nil
}

// PullRequestFiles reads one bounded page of a pull request's changed files. Truncated
// reports more pages — which is also how the vendor's own hard ceiling of 3000 listed
// files surfaces: the pages simply end there.
func (c *Client) PullRequestFiles(
	ctx context.Context, token string, repository int64, number, limit int,
) ([]ChangedFile, bool, error) {
	var decoded []fileJSON
	header, err := c.repoRead(ctx, token, repository,
		"/pulls/"+strconv.Itoa(number)+"/files",
		url.Values{"per_page": {strconv.Itoa(limit)}}, &decoded)
	if err != nil {
		return nil, false, err
	}
	return changedFiles(decoded), hasNextPage(header), nil
}

// CheckRun is one CI check as the vendor reports it against a commit.
type CheckRun struct {
	Name       string
	Status     string
	Conclusion string
}

// CheckRuns reads the check runs against one ref, bounded.
func (c *Client) CheckRuns(
	ctx context.Context, token string, repository int64, ref string, limit int,
) ([]CheckRun, bool, error) {
	var decoded struct {
		Total  int `json:"total_count"`
		Checks []struct {
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"check_runs"`
	}
	_, err := c.repoRead(ctx, token, repository,
		"/commits/"+url.PathEscape(ref)+"/check-runs",
		url.Values{"per_page": {strconv.Itoa(limit)}}, &decoded)
	if err != nil {
		return nil, false, err
	}
	checks := make([]CheckRun, 0, len(decoded.Checks))
	for _, one := range decoded.Checks {
		checks = append(checks, CheckRun(one))
	}
	return checks, decoded.Total > len(checks), nil
}

// WorkflowRun is one Actions run as the listing reports it.
type WorkflowRun struct {
	ID         int64
	Name       string
	Branch     string
	HeadSHA    string
	Event      string
	Status     string
	Conclusion string
	CreatedAt  string
	HTMLURL    string
}

// WorkflowRuns is a bounded read of a repository's Actions runs.
type WorkflowRuns struct {
	Runs      []WorkflowRun
	Truncated bool
}

// RunsQuery bounds one workflow-run read; the window travels as the vendor's own
// created-range filter.
type RunsQuery struct {
	RepositoryID int64
	Since        time.Time
	Until        time.Time
	Limit        int
}

// WorkflowRuns reads a repository's Actions runs inside a window, newest first.
func (c *Client) WorkflowRuns(
	ctx context.Context, token string, query RunsQuery,
) (WorkflowRuns, error) {
	parameters := url.Values{"per_page": {strconv.Itoa(query.Limit)}}
	if created := createdRange(query.Since, query.Until); created != "" {
		parameters.Set("created", created)
	}
	var decoded struct {
		Total int `json:"total_count"`
		Runs  []struct {
			ID         int64  `json:"id"`
			Name       string `json:"name"`
			HeadBranch string `json:"head_branch"`
			HeadSHA    string `json:"head_sha"`
			Event      string `json:"event"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			CreatedAt  string `json:"created_at"`
			HTMLURL    string `json:"html_url"`
		} `json:"workflow_runs"`
	}
	_, err := c.repoRead(ctx, token, query.RepositoryID, "/actions/runs", parameters, &decoded)
	if err != nil {
		return WorkflowRuns{}, err
	}
	read := WorkflowRuns{
		Runs:      make([]WorkflowRun, 0, len(decoded.Runs)),
		Truncated: decoded.Total > len(decoded.Runs),
	}
	for _, one := range decoded.Runs {
		read.Runs = append(read.Runs, WorkflowRun{
			ID: one.ID, Name: one.Name, Branch: one.HeadBranch, HeadSHA: one.HeadSHA,
			Event: one.Event, Status: one.Status, Conclusion: one.Conclusion,
			CreatedAt: one.CreatedAt, HTMLURL: one.HTMLURL,
		})
	}
	return read, nil
}

// createdRange renders a window as the vendor's created filter. Half-open asks are
// rendered with the vendor's own comparison forms.
func createdRange(since, until time.Time) string {
	const form = "2006-01-02T15:04:05Z"
	switch {
	case since.IsZero() && until.IsZero():
		return ""
	case since.IsZero():
		return "<=" + until.UTC().Format(form)
	case until.IsZero():
		return ">=" + since.UTC().Format(form)
	default:
		return since.UTC().Format(form) + ".." + until.UTC().Format(form)
	}
}

// Job is one job of a workflow run, with the step that failed when one did.
type Job struct {
	ID         int64
	Name       string
	Status     string
	Conclusion string
	FailedStep string
}

// RunJobs reads one run's jobs, bounded by the vendor's page.
func (c *Client) RunJobs(
	ctx context.Context, token string, repository, run int64, limit int,
) ([]Job, error) {
	var decoded struct {
		Jobs []struct {
			ID         int64  `json:"id"`
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			Steps      []struct {
				Name       string `json:"name"`
				Conclusion string `json:"conclusion"`
			} `json:"steps"`
		} `json:"jobs"`
	}
	_, err := c.repoRead(ctx, token, repository,
		"/actions/runs/"+strconv.FormatInt(run, 10)+"/jobs",
		url.Values{"per_page": {strconv.Itoa(limit)}}, &decoded)
	if err != nil {
		return nil, err
	}
	jobs := make([]Job, 0, len(decoded.Jobs))
	for _, one := range decoded.Jobs {
		job := Job{
			ID: one.ID, Name: one.Name, Status: one.Status, Conclusion: one.Conclusion,
		}
		for _, step := range one.Steps {
			if step.Conclusion == "failure" {
				job.FailedStep = step.Name
				break
			}
		}
		jobs = append(jobs, job)
	}
	return jobs, nil
}

// JobLog reads the tail of one job's log. The endpoint 302s to a storage URL that
// expires in about a minute, so the log is fetched in this one read: the tail is asked
// for with a Range request, and a storage that ignores Range is read up to the large
// bound and tailed locally — truncation flagged either way.
func (c *Client) JobLog(
	ctx context.Context, token string, repository, job int64, tailBytes int,
) (string, bool, error) {
	var body []byte
	var status int
	var header http.Header
	err := c.withRepoName(ctx, token, repository, func(name string) error {
		answered, code, head, err := c.rawCall(ctx, token, fetchSpec{
			path: "/repos/" + name + "/actions/jobs/" +
				strconv.FormatInt(job, 10) + "/logs",
			rangeHeader: "bytes=-" + strconv.Itoa(tailBytes),
			bound:       maxLargeResponseBytes,
		})
		if err != nil {
			return err
		}
		body, status, header = answered, code, head
		return nil
	})
	if err != nil {
		return "", false, err
	}
	truncated := partialOmitsHead(status, header)
	if len(body) > tailBytes {
		body = tailAtLine(body, tailBytes)
		truncated = true
	}
	return string(body), truncated, nil
}

// tailAtLine keeps the last tailBytes of a log, advanced to the next whole line so the
// tail does not open mid-word — unless no line boundary falls inside the slice, where a
// cut line beats an empty answer.
func tailAtLine(body []byte, tailBytes int) []byte {
	tail := body[len(body)-tailBytes:]
	if newline := bytes.IndexByte(tail, '\n'); newline >= 0 && newline+1 < len(tail) {
		return tail[newline+1:]
	}
	return tail
}

// FileContent is one file at a ref, bounded.
type FileContent struct {
	Path      string
	Ref       string
	Content   string
	Truncated bool
}

// File reads one file's contents at a ref over the raw media type — the JSON contents
// path caps at 1 MB while raw serves up to 100, and this read wants neither whole: it
// reads maxBytes and flags the rest.
func (c *Client) File(
	ctx context.Context, token string, repository int64, path, ref string, maxBytes int,
) (FileContent, error) {
	parameters := url.Values{}
	if ref != "" {
		parameters.Set("ref", ref)
	}
	var body []byte
	err := c.withRepoName(ctx, token, repository, func(name string) error {
		answered, _, _, err := c.rawCall(ctx, token, fetchSpec{
			path:       "/repos/" + name + "/contents/" + path,
			parameters: parameters,
			accept:     "application/vnd.github.raw+json",
			bound:      maxLargeResponseBytes,
		})
		body = answered
		return err
	})
	if err != nil {
		if errors.Is(err, ErrResponseTooLarge) {
			// The large bound itself refused: the file outgrows anything this read
			// would carry a prefix of honestly.
			return FileContent{}, fmt.Errorf(
				"the file at %s is larger than %d bytes and cannot be read by this "+
					"tool; read a more specific file instead", path, maxLargeResponseBytes)
		}
		return FileContent{}, err
	}
	content := FileContent{Path: path, Ref: ref}
	if len(body) > maxBytes {
		body = body[:maxBytes]
		content.Truncated = true
	}
	content.Content = string(body)
	return content, nil
}

// Release is one release as the listing reports it. Bare un-released tags are not
// listed by the vendor and so not by this.
type Release struct {
	Name        string
	Tag         string
	PublishedAt string
	Author      string
	Prerelease  bool
	HTMLURL     string
}

// Releases is a bounded read of what shipped.
type Releases struct {
	Releases  []Release
	Truncated bool
}

// Releases lists a repository's releases, newest first, bounded.
func (c *Client) Releases(
	ctx context.Context, token string, repository int64, limit int,
) (Releases, error) {
	var decoded []struct {
		Name        string `json:"name"`
		TagName     string `json:"tag_name"`
		PublishedAt string `json:"published_at"`
		Prerelease  bool   `json:"prerelease"`
		HTMLURL     string `json:"html_url"`
		Author      *struct {
			Login string `json:"login"`
		} `json:"author"`
	}
	header, err := c.repoRead(ctx, token, repository, "/releases",
		url.Values{"per_page": {strconv.Itoa(limit)}}, &decoded)
	if err != nil {
		return Releases{}, err
	}
	read := Releases{
		Releases:  make([]Release, 0, len(decoded)),
		Truncated: hasNextPage(header),
	}
	for _, one := range decoded {
		release := Release{
			Name: one.Name, Tag: one.TagName, PublishedAt: one.PublishedAt,
			Prerelease: one.Prerelease, HTMLURL: one.HTMLURL,
		}
		if one.Author != nil {
			release.Author = one.Author.Login
		}
		read.Releases = append(read.Releases, release)
	}
	return read, nil
}

// fileJSON is the vendor's changed-file shape, decoded once for commits and pulls.
type fileJSON struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Patch     string `json:"patch"`
}

func changedFiles(files []fileJSON) []ChangedFile {
	changed := make([]ChangedFile, 0, len(files))
	for _, one := range files {
		changed = append(changed, ChangedFile{
			Path: one.Filename, Status: one.Status,
			Additions: one.Additions, Deletions: one.Deletions, Patch: one.Patch,
		})
	}
	return changed
}
