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
	"time"
)

// defaultBaseURL is where the REST API lives. A test points the client at a fake instead,
// which is the provider transport seam: the verification and tool code under test is the
// real code, and only the far end is stood in for.
const defaultBaseURL = "https://api.github.com"

// maxResponseBytes bounds what one answer may hold. GitHub's own page limits keep real
// answers far below this; an answer that reaches it is not the API this client speaks.
const maxResponseBytes = 4 << 20

// requestTimeout is a backstop on one call. Every caller passes a bounded context; this
// exists so a caller that forgot cannot hold a connection forever.
const requestTimeout = 60 * time.Second

// maxRetryWait bounds how long a Retry-After is worth honouring. A vendor asking for
// more is answered as rate-limited now: one bounded read must not park a goroutine on
// the vendor's say-so, deadline or none.
const maxRetryWait = 30 * time.Second

// ErrRateLimited reports that GitHub refused the call for rate: either twice in a row, or
// with an exhausted hourly budget that is not worth sleeping towards. Every call this
// client makes is a read, so repeating one later is safe.
var ErrRateLimited = errors.New("github is rate limiting this app")

// APIError is GitHub's own refusal: the status and the message the API named.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return "github answered " + strconv.Itoa(e.Status) + ": " + e.Message
}

// Client is the one HTTP client this provider holds. One per vendor, deliberately: a
// second client is a second place a header, a bound or a retry rule could differ.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient builds the client. An empty base URL means the vendor's own.
func NewClient(baseURL string) *Client {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &Client{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		http:    &http.Client{Timeout: requestTimeout},
	}
}

// Installation is one App installation as GitHub reports it: whose account it is, whether
// it is suspended, and whether it selected repositories or granted all of them.
type Installation struct {
	Account             string
	AccountType         string
	Suspended           bool
	RepositorySelection string
}

// Repository is one repository as the installation sees it. The numeric ID is the stable
// identity everything downstream stores: it survives renames and transfers, which names
// do not.
type Repository struct {
	ID            int64
	Name          string
	FullName      string
	Private       bool
	Archived      bool
	DefaultBranch string
	Description   string
}

// Repositories is a bounded page of the installation's selected repositories.
type Repositories struct {
	Repositories []Repository
	Truncated    bool
}

// Commit is one commit as the listing reports it.
type Commit struct {
	SHA     string
	Message string
	// Author is the GitHub login when one is linked, and the git author name otherwise.
	Author   string
	AuthorAt string
}

// Commits is a bounded read of a repository's history.
type Commits struct {
	Commits   []Commit
	Truncated bool
}

// CommitsQuery bounds one history read. The window is the incident's own.
type CommitsQuery struct {
	RepositoryID int64
	Since        time.Time
	Until        time.Time
	Limit        int
}

// PullRequest is one pull request as the listing reports it.
type PullRequest struct {
	Number    int
	Title     string
	State     string
	MergedAt  string
	UpdatedAt string
	Author    string
	Head      string
	Base      string
}

// PullRequests is a bounded read of a repository's pull requests, most recently updated
// first.
type PullRequests struct {
	PullRequests []PullRequest
	Truncated    bool
}

// PullsQuery bounds one pull-request read.
type PullsQuery struct {
	RepositoryID int64
	Limit        int
}

// Installation reads one installation under the App's own JWT. It is the probe behind
// "verified means the far end answered": an installation GitHub does not know, or one it
// suspended, is the operator's answer.
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

// mintInstallationToken asks for a token scoped to one installation's grant. It is a POST
// and still safe to repeat: minting again invalidates nothing and grants nothing new.
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

// Repositories lists what the installation selected, one bounded page, by stable IDs.
func (c *Client) Repositories(
	ctx context.Context, token string, limit int,
) (Repositories, error) {
	var decoded struct {
		TotalCount   int              `json:"total_count"`
		Repositories []repositoryJSON `json:"repositories"`
	}
	_, err := c.call(ctx, token, http.MethodGet, "/installation/repositories",
		url.Values{"per_page": {strconv.Itoa(limit)}}, &decoded)
	if err != nil {
		return Repositories{}, err
	}

	listed := Repositories{
		Repositories: make([]Repository, 0, len(decoded.Repositories)),
		Truncated:    decoded.TotalCount > len(decoded.Repositories),
	}
	for _, one := range decoded.Repositories {
		listed.Repositories = append(listed.Repositories, one.repository())
	}
	return listed, nil
}

// Commits reads a repository's history inside a window, bounded, newest first.
func (c *Client) Commits(
	ctx context.Context, token string, query CommitsQuery,
) (Commits, error) {
	parameters := url.Values{"per_page": {strconv.Itoa(query.Limit)}}
	if !query.Since.IsZero() {
		parameters.Set("since", query.Since.UTC().Format(time.RFC3339))
	}
	if !query.Until.IsZero() {
		parameters.Set("until", query.Until.UTC().Format(time.RFC3339))
	}

	var decoded []struct {
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
	}
	header, err := c.call(ctx, token, http.MethodGet,
		"/repositories/"+strconv.FormatInt(query.RepositoryID, 10)+"/commits",
		parameters, &decoded)
	var refusal *APIError
	if errors.As(err, &refusal) && refusal.Status == http.StatusConflict {
		// GitHub's answer for a repository with no commits yet. An empty history is an
		// answer, not a failure.
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
		commit := Commit{
			SHA:      one.SHA,
			Message:  one.Commit.Message,
			Author:   one.Commit.Author.Name,
			AuthorAt: one.Commit.Author.Date,
		}
		if one.Author != nil && one.Author.Login != "" {
			commit.Author = one.Author.Login
		}
		read.Commits = append(read.Commits, commit)
	}
	return read, nil
}

// PullRequests reads a repository's pull requests, most recently updated first, in every
// state — a merged pull request is exactly the change context an investigation wants.
func (c *Client) PullRequests(
	ctx context.Context, token string, query PullsQuery,
) (PullRequests, error) {
	var decoded []struct {
		Number    int     `json:"number"`
		Title     string  `json:"title"`
		State     string  `json:"state"`
		MergedAt  *string `json:"merged_at"`
		UpdatedAt string  `json:"updated_at"`
		User      *struct {
			Login string `json:"login"`
		} `json:"user"`
		Head struct {
			Ref string `json:"ref"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}
	header, err := c.call(ctx, token, http.MethodGet,
		"/repositories/"+strconv.FormatInt(query.RepositoryID, 10)+"/pulls",
		url.Values{
			"state":     {"all"},
			"sort":      {"updated"},
			"direction": {"desc"},
			"per_page":  {strconv.Itoa(query.Limit)},
		}, &decoded)
	if err != nil {
		return PullRequests{}, err
	}

	read := PullRequests{
		PullRequests: make([]PullRequest, 0, len(decoded)),
		Truncated:    hasNextPage(header),
	}
	for _, one := range decoded {
		pull := PullRequest{
			Number: one.Number, Title: one.Title, State: one.State,
			UpdatedAt: one.UpdatedAt, Head: one.Head.Ref, Base: one.Base.Ref,
		}
		if one.MergedAt != nil {
			pull.MergedAt = *one.MergedAt
		}
		if one.User != nil {
			pull.Author = one.User.Login
		}
		read.PullRequests = append(read.PullRequests, pull)
	}
	return read, nil
}

// repositoryJSON is the vendor's repository shape, decoded once for both reads.
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

// call performs one REST operation and decodes the answer into out, returning the
// response headers for the callers that read pagination from them.
//
// A rate refusal carrying Retry-After is retried exactly once, and only when the wait
// fits the caller's own deadline; an exhausted hourly budget is answered as ErrRateLimited
// immediately, because its reset is not worth sleeping towards. Every operation here is
// a read or an idempotent mint, so the retry cannot double an effect.
func (c *Client) call(
	ctx context.Context, credential, method, path string, parameters url.Values, out any,
) (http.Header, error) {
	for attempt := 0; ; attempt++ {
		header, wait, err := c.once(ctx, credential, method, path, parameters, out)
		if wait == 0 || attempt == 1 {
			if wait != 0 {
				return nil, fmt.Errorf("%w: %s answered for rate twice", ErrRateLimited, path)
			}
			return header, err
		}

		if wait > maxRetryWait {
			return nil, fmt.Errorf("%w: %s asked for a %s wait, past what one read may park",
				ErrRateLimited, path, wait)
		}
		deadline, bounded := ctx.Deadline()
		if bounded && time.Now().Add(wait).After(deadline) {
			return nil, fmt.Errorf("%w: %s asked for a %s wait, past this call's deadline",
				ErrRateLimited, path, wait)
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// once performs one attempt. A non-zero wait reports a retryable rate refusal; everything
// else is the final answer.
func (c *Client) once(
	ctx context.Context, credential, method, path string, parameters url.Values, out any,
) (http.Header, time.Duration, error) {
	address := c.baseURL + path
	if len(parameters) > 0 {
		address += "?" + parameters.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, method, address, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("building the %s request: %w", path, err)
	}
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	response, err := c.http.Do(request)
	if err != nil {
		// The transport error is wrapped, not quoted onward to an operator: url.Error
		// carries the full URL, and the caller decides what an operator sees.
		return nil, 0, fmt.Errorf("reaching github for %s: %w", path, err)
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, 0, fmt.Errorf("reading the %s answer: %w", path, err)
	}
	if len(body) > maxResponseBytes {
		return nil, 0, fmt.Errorf("the %s answer exceeds %d bytes; refusing to read further",
			path, maxResponseBytes)
	}

	if wait, limited := rateRefusal(response); limited {
		if wait == 0 {
			// The hourly budget: no named wait, a reset up to an hour away and not
			// worth sleeping towards.
			return nil, 0, fmt.Errorf("%w: the hourly budget is exhausted", ErrRateLimited)
		}
		return nil, wait, nil
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		var refusal struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &refusal)
		if refusal.Message == "" {
			refusal.Message = "no reason given"
		}
		return nil, 0, &APIError{Status: response.StatusCode, Message: refusal.Message}
	}

	if err := json.Unmarshal(body, out); err != nil {
		return nil, 0, fmt.Errorf("decoding the %s answer: %w", path, err)
	}
	return response.Header, 0, nil
}

// rateRefusal reads GitHub's two rate-limit shapes: a secondary limit naming a wait worth
// taking once, and the primary hourly budget, reported with no wait because its reset is
// its own clock. A plain 403 is a permissions refusal and is not read as either.
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

// hasNextPage reads whether the vendor holds more than this page, from the Link header
// GitHub paginates with.
func hasNextPage(header http.Header) bool {
	return strings.Contains(header.Get("Link"), `rel="next"`)
}
