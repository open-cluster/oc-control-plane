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

const maxLargeResponseBytes = 16 << 20

const commitFilePageLimit = 300

type ChangedFile struct {
	Path      string
	Status    string
	Additions int
	Deletions int
	Patch     string
}

type CommitDetail struct {
	SHA            string
	Message        string
	Author         string
	AuthorAt       string
	HTMLURL        string
	Files          []ChangedFile
	FilesTruncated bool
}

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

type CheckRun struct {
	Name       string
	Status     string
	Conclusion string
}

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

type WorkflowRuns struct {
	Runs      []WorkflowRun
	Truncated bool
}

type RunsQuery struct {
	RepositoryID int64
	Since        time.Time
	Until        time.Time
	Limit        int
}

func (c *Client) WorkflowRuns(
	ctx context.Context, token string, query RunsQuery,
) (WorkflowRuns, error) {
	parameters := url.Values{"per_page": {strconv.Itoa(query.Limit)}}
	created, empty := createdRange(query.Since, query.Until)
	if empty {
		return WorkflowRuns{}, nil
	}
	if created != "" {
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

func createdRange(since, until time.Time) (string, bool) {
	const form = "2006-01-02T15:04:05Z"
	if !since.IsZero() {
		since = since.Add(time.Second - time.Nanosecond).Truncate(time.Second)
	}
	if !until.IsZero() {
		until = until.Add(-time.Nanosecond).Truncate(time.Second)
	}
	if !since.IsZero() && !until.IsZero() && since.After(until) {
		return "", true
	}
	switch {
	case since.IsZero() && until.IsZero():
		return "", false
	case since.IsZero():
		return "<=" + until.UTC().Format(form), false
	case until.IsZero():
		return ">=" + since.UTC().Format(form), false
	default:
		return since.UTC().Format(form) + ".." + until.UTC().Format(form), false
	}
}

type Job struct {
	ID         int64
	Name       string
	Status     string
	Conclusion string
	FailedStep string
}

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

func tailAtLine(body []byte, tailBytes int) []byte {
	tail := body[len(body)-tailBytes:]
	if newline := bytes.IndexByte(tail, '\n'); newline >= 0 && newline+1 < len(tail) {
		return tail[newline+1:]
	}
	return tail
}

type FileContent struct {
	Path      string
	Ref       string
	Content   string
	Truncated bool
}

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

type Release struct {
	Name        string
	Tag         string
	PublishedAt string
	Author      string
	Prerelease  bool
	HTMLURL     string
}

type Releases struct {
	Releases  []Release
	Truncated bool
}

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
