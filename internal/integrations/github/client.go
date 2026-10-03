package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultBaseURL = "https://api.github.com"

const maxResponseBytes = 4 << 20

const requestTimeout = 60 * time.Second

const maxRetryWait = 30 * time.Second

var ErrRateLimited = errors.New("github is rate limiting this app")

var ErrAPIVersionRetired = errors.New(
	"github no longer serves the API version this build pins; update the control plane")

var ErrRepositoryNotSelected = errors.New(
	"this repository is not among the ones the app installation selected; add it in the " +
		"installation's settings in github")

var ErrResponseTooLarge = errors.New("the answer exceeds this read's bound")

type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return "github answered " + strconv.Itoa(e.Status) + ": " + e.Message
}

type Client struct {
	baseURL string
	http    *http.Client

	mu    sync.Mutex
	names map[int64]string
}

func NewClient(baseURL string) *Client {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &Client{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		http:    &http.Client{Timeout: requestTimeout},
		names:   map[int64]string{},
	}
}

func (c *Client) reachesTheVendorsOwnAPI() bool { return c != nil && c.baseURL == defaultBaseURL }

type Installation struct {
	Account             string
	AccountType         string
	Suspended           bool
	RepositorySelection string
}

func (c *Client) App(ctx context.Context, jwt string) (string, error) {
	var decoded struct {
		Slug string `json:"slug"`
	}
	_, err := c.call(ctx, jwt, http.MethodGet, "/app", nil, &decoded)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(decoded.Slug) == "" {
		return "", errors.New("github returned no app slug")
	}
	return decoded.Slug, nil
}

type Repository struct {
	ID            int64
	Name          string
	FullName      string
	Private       bool
	Archived      bool
	DefaultBranch string
	Description   string
}

type Repositories struct {
	Repositories []Repository
	Truncated    bool
	NextPage     bool
}

type Commit struct {
	SHA      string
	Message  string
	Author   string
	AuthorAt string
	HTMLURL  string
}

type Commits struct {
	Commits   []Commit
	Truncated bool
}

type CommitsQuery struct {
	RepositoryID int64
	Since        time.Time
	Until        time.Time
	Limit        int
}

func (c *Client) Installation(
	ctx context.Context, jwt string, installation int64,
) (Installation, error) {
	var decoded struct {
		Account struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"account"`
		SuspendedAt         *string `json:"suspended_at"`
		RepositorySelection string  `json:"repository_selection"`
	}
	_, err := c.call(ctx, jwt, http.MethodGet,
		"/app/installations/"+strconv.FormatInt(installation, 10), nil, &decoded)
	if err != nil {
		return Installation{}, err
	}
	return Installation{
		Account:             decoded.Account.Login,
		AccountType:         decoded.Account.Type,
		Suspended:           decoded.SuspendedAt != nil,
		RepositorySelection: decoded.RepositorySelection,
	}, nil
}

type UserInstallation struct {
	ID      int64
	Account string
}

func (c *Client) UserInstallations(
	ctx context.Context, token string, page int,
) ([]UserInstallation, bool, error) {
	var decoded struct {
		Installations []struct {
			ID      int64 `json:"id"`
			Account struct {
				Login string `json:"login"`
			} `json:"account"`
		} `json:"installations"`
	}
	parameters := url.Values{"per_page": {"100"}}
	if page > 1 {
		parameters.Set("page", strconv.Itoa(page))
	}
	header, err := c.call(ctx, token, http.MethodGet, "/user/installations",
		parameters, &decoded)
	if err != nil {
		return nil, false, err
	}

	reachable := make([]UserInstallation, 0, len(decoded.Installations))
	for _, one := range decoded.Installations {
		reachable = append(reachable, UserInstallation{ID: one.ID, Account: one.Account.Login})
	}
	return reachable, hasNextPage(header), nil
}

func (c *Client) mintInstallationToken(
	ctx context.Context, jwt string, installation int64,
) (installationToken, error) {
	var decoded struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	_, err := c.call(ctx, jwt, http.MethodPost,
		"/app/installations/"+strconv.FormatInt(installation, 10)+"/access_tokens",
		nil, &decoded)
	if err != nil {
		return installationToken{}, err
	}
	return installationToken{token: decoded.Token, expires: decoded.ExpiresAt}, nil
}

func (c *Client) Repositories(
	ctx context.Context, token string, limit, page int,
) (Repositories, error) {
	var decoded struct {
		TotalCount   int              `json:"total_count"`
		Repositories []repositoryJSON `json:"repositories"`
	}
	parameters := url.Values{"per_page": {strconv.Itoa(limit)}}
	if page > 1 {
		parameters.Set("page", strconv.Itoa(page))
	}
	header, err := c.call(ctx, token, http.MethodGet, "/installation/repositories",
		parameters, &decoded)
	if err != nil {
		return Repositories{}, err
	}

	listed := Repositories{
		Repositories: make([]Repository, 0, len(decoded.Repositories)),
		Truncated:    hasNextPage(header) || decoded.TotalCount > len(decoded.Repositories),
		NextPage:     hasNextPage(header),
	}
	for _, one := range decoded.Repositories {
		listed.Repositories = append(listed.Repositories, one.repository())
	}
	return listed, nil
}

func (c *Client) Commits(
	ctx context.Context, token string, query CommitsQuery,
) (Commits, error) {
	parameters := url.Values{"per_page": {strconv.Itoa(query.Limit)}}
	// GitHub's since bound is exclusive and its timestamps are second-granular; widen both
	// bounds before filtering exact commit times locally.
	if !query.Since.IsZero() {
		parameters.Set("since", query.Since.Add(-time.Second).UTC().Format(time.RFC3339))
	}
	if !query.Until.IsZero() {
		parameters.Set("until", query.Until.Add(time.Second-time.Nanosecond).UTC().Format(time.RFC3339))
	}

	var decoded []struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message string `json:"message"`
			Author  struct {
				Name string `json:"name"`
				Date string `json:"date"`
			} `json:"author"`
			Committer struct {
				Date string `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
		Author *struct {
			Login string `json:"login"`
		} `json:"author"`
		HTMLURL string `json:"html_url"`
	}
	header, err := c.repoRead(ctx, token, query.RepositoryID, "/commits", parameters, &decoded)
	var refusal *APIError
	if errors.As(err, &refusal) && refusal.Status == http.StatusConflict {
		return Commits{}, nil
	}
	if err != nil {
		return Commits{}, err
	}

	read := Commits{
		Commits:   make([]Commit, 0, len(decoded)),
		Truncated: hasNextPage(header),
	}
	for _, one := range decoded {
		if !query.Since.IsZero() || !query.Until.IsZero() {
			at, parseErr := time.Parse(time.RFC3339Nano, one.Commit.Committer.Date)
			if parseErr != nil {
				return Commits{}, errors.New("GitHub returned a commit without a valid commit timestamp")
			}
			if (!query.Since.IsZero() && at.Before(query.Since)) || (!query.Until.IsZero() && !at.Before(query.Until)) {
				continue
			}
		}
		commit := Commit{
			SHA:      one.SHA,
			Message:  one.Commit.Message,
			Author:   one.Commit.Author.Name,
			AuthorAt: one.Commit.Author.Date,
			HTMLURL:  one.HTMLURL,
		}
		if one.Author != nil && one.Author.Login != "" {
			commit.Author = one.Author.Login
		}
		read.Commits = append(read.Commits, commit)
	}
	return read, nil
}

type repositoryJSON struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	Private       bool   `json:"private"`
	Archived      bool   `json:"archived"`
	DefaultBranch string `json:"default_branch"`
	Description   string `json:"description"`
}

func (r repositoryJSON) repository() Repository { return Repository(r) }

const maxResolvePages = 10

func (c *Client) repoRead(
	ctx context.Context, token string, repository int64, suffix string,
	parameters url.Values, out any,
) (http.Header, error) {
	return c.repoReadBounded(ctx, token, repository, suffix, parameters, 0, out)
}

func (c *Client) repoReadBounded(
	ctx context.Context, token string, repository int64, suffix string,
	parameters url.Values, bound int, out any,
) (http.Header, error) {
	var header http.Header
	err := c.withRepoName(ctx, token, repository, func(name string) error {
		body, _, answered, err := c.rawCall(ctx, token, fetchSpec{
			path: "/repos/" + name + suffix, parameters: parameters, bound: bound,
		})
		if err != nil {
			return err
		}
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("decoding the %s answer: %w", suffix, err)
		}
		header = answered
		return nil
	})
	return header, err
}

func (c *Client) withRepoName(
	ctx context.Context, token string, repository int64, read func(name string) error,
) error {
	name, err := c.fullName(ctx, token, repository, false)
	if err != nil {
		return err
	}
	err = read(name)
	var refusal *APIError
	if errors.As(err, &refusal) &&
		(refusal.Status == http.StatusNotFound || refusal.Status == http.StatusMovedPermanently) {
		fresh, resolveErr := c.fullName(ctx, token, repository, true)
		if resolveErr != nil || fresh == name {
			return err
		}
		return read(fresh)
	}
	return err
}

func (c *Client) fullName(
	ctx context.Context, token string, repository int64, refresh bool,
) (string, error) {
	if !refresh {
		c.mu.Lock()
		name, cached := c.names[repository]
		c.mu.Unlock()
		if cached {
			return name, nil
		}
	}

	for page := 1; page <= maxResolvePages; page++ {
		var decoded struct {
			Repositories []repositoryJSON `json:"repositories"`
		}
		header, err := c.call(ctx, token, http.MethodGet, "/installation/repositories",
			url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}}, &decoded)
		if err != nil {
			return "", err
		}
		c.mu.Lock()
		for _, one := range decoded.Repositories {
			c.names[one.ID] = one.FullName
		}
		name, found := c.names[repository]
		c.mu.Unlock()
		if found {
			return name, nil
		}
		if !hasNextPage(header) {
			break
		}
	}
	return "", fmt.Errorf("%w (repository %d)", ErrRepositoryNotSelected, repository)
}

func (c *Client) call(
	ctx context.Context, credential, method, path string, parameters url.Values, out any,
) (http.Header, error) {
	body, _, header, err := c.rawCall(ctx, credential, fetchSpec{
		method: method, path: path, parameters: parameters,
	})
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return nil, fmt.Errorf("decoding the %s answer: %w", path, err)
	}
	return header, nil
}

type fetchSpec struct {
	method      string
	path        string
	parameters  url.Values
	accept      string
	rangeHeader string
	bound       int
}

func (c *Client) fetch(
	ctx context.Context, credential string, spec fetchSpec,
) ([]byte, int, http.Header, time.Duration, error) {
	address := c.baseURL + spec.path
	if len(spec.parameters) > 0 {
		address += "?" + spec.parameters.Encode()
	}
	method := spec.method
	if method == "" {
		method = http.MethodGet
	}
	request, err := http.NewRequestWithContext(ctx, method, address, nil)
	if err != nil {
		return nil, 0, nil, 0, fmt.Errorf("building the %s request: %w", spec.path, err)
	}
	// Go drops Authorization when GitHub redirects log downloads to another host.
	request.Header.Set("Authorization", "Bearer "+credential)
	accept := spec.accept
	if accept == "" {
		accept = "application/vnd.github+json"
	}
	request.Header.Set("Accept", accept)
	if spec.rangeHeader != "" {
		request.Header.Set("Range", spec.rangeHeader)
	}
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	response, err := c.http.Do(request)
	if err != nil {
		return nil, 0, nil, 0, fmt.Errorf("reaching github for %s: %w", spec.path, err)
	}
	defer func() { _ = response.Body.Close() }()

	bound := spec.bound
	if bound == 0 {
		bound = maxResponseBytes
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, int64(bound)+1))
	if err != nil {
		return nil, 0, nil, 0, fmt.Errorf("reading the %s answer: %w", spec.path, err)
	}
	if len(body) > bound {
		return nil, 0, nil, 0, fmt.Errorf(
			"%w: the %s answer exceeds %d bytes; refusing to read further",
			ErrResponseTooLarge, spec.path, bound)
	}

	if wait, limited := rateRefusal(response); limited {
		if wait == 0 {
			return nil, 0, nil, 0, fmt.Errorf("%w: the hourly budget is exhausted", ErrRateLimited)
		}
		return nil, 0, nil, wait, nil
	}
	if response.StatusCode == http.StatusGone {
		return nil, 0, nil, 0, fmt.Errorf("%w (%s answered 410)", ErrAPIVersionRetired, spec.path)
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		var refusal struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &refusal)
		if refusal.Message == "" {
			refusal.Message = "no reason given"
		}
		return nil, 0, nil, 0, &APIError{Status: response.StatusCode, Message: refusal.Message}
	}
	return body, response.StatusCode, response.Header, 0, nil
}

func (c *Client) rawCall(
	ctx context.Context, credential string, spec fetchSpec,
) ([]byte, int, http.Header, error) {
	for attempt := 0; ; attempt++ {
		body, status, header, wait, err := c.fetch(ctx, credential, spec)
		if wait == 0 || attempt == 1 {
			if wait != 0 {
				return nil, 0, nil, fmt.Errorf("%w: %s answered for rate twice",
					ErrRateLimited, spec.path)
			}
			return body, status, header, err
		}
		if wait > maxRetryWait {
			return nil, 0, nil, fmt.Errorf("%w: %s asked for a %s wait, past what one read may park",
				ErrRateLimited, spec.path, wait)
		}
		deadline, bounded := ctx.Deadline()
		if bounded && time.Now().Add(wait).After(deadline) {
			return nil, 0, nil, fmt.Errorf("%w: %s asked for a %s wait, past this call's deadline",
				ErrRateLimited, spec.path, wait)
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil, 0, nil, ctx.Err()
		}
	}
}

func rateRefusal(response *http.Response) (time.Duration, bool) {
	tooMany := response.StatusCode == http.StatusTooManyRequests
	exhausted := response.StatusCode == http.StatusForbidden &&
		response.Header.Get("X-RateLimit-Remaining") == "0"
	if !tooMany && !exhausted {
		return 0, false
	}

	seconds, err := strconv.Atoi(strings.TrimSpace(response.Header.Get("Retry-After")))
	if err == nil && seconds >= 1 {
		return time.Duration(seconds) * time.Second, true
	}
	if exhausted {
		return 0, true
	}
	return time.Second, true
}

func hasNextPage(header http.Header) bool {
	return strings.Contains(header.Get("Link"), `rel="next"`)
}

func partialOmitsHead(status int, header http.Header) bool {
	if status != http.StatusPartialContent {
		return false
	}
	rest, ranged := strings.CutPrefix(header.Get("Content-Range"), "bytes ")
	if !ranged {
		return true
	}
	start, _, dashed := strings.Cut(rest, "-")
	if !dashed {
		return true
	}
	return strings.TrimSpace(start) != "0"
}
