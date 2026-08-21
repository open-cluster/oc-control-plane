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

// ErrAPIVersionRetired reports a 410 against the pinned API version: GitHub has stopped
// serving it, and the fix is a control-plane update, not a configuration change. Mapped
// distinctly so the eventual cliff self-diagnoses instead of reading as a missing
// resource.
var ErrAPIVersionRetired = errors.New(
	"github no longer serves the API version this build pins; update the control plane")

// ErrRepositoryNotSelected reports a repository outside what the installation selected.
// A read that names one is answered with this and nothing else happens: no other
// credential is tried, and no attempt is repeated. The reason is the whole answer, because
// the gap in an investigation is explained by it.
var ErrRepositoryNotSelected = errors.New(
	"this repository is not among the ones the app installation selected; add it in the " +
		"installation's settings in github")

// ErrResponseTooLarge reports an answer past the read's bound: nothing of it is kept,
// because a silently cut prefix of an unbounded answer is not an honest read. Callers
// that can say something more useful than the raw refusal match on this.
var ErrResponseTooLarge = errors.New("the answer exceeds this read's bound")

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

	// names caches repository full names by stable numeric id, for the documented
	// /repos/{owner}/{repo} routes. The id is the stored identity because it survives
	// renames; the name is resolved from the installation's own repository listing and
	// re-resolved once when a read answers 404 or 301 — which is what a rename looks
	// like from here.
	mu    sync.Mutex
	names map[int64]string
}

// NewClient builds the client. An empty base URL means the vendor's own.
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
	// Truncated says the installation selected more than this page carries; NextPage
	// says the vendor serves a further page, which is what a walk continues on.
	Truncated bool
	NextPage  bool
}

// Commit is one commit as the listing reports it.
type Commit struct {
	SHA     string
	Message string
	// Author is the GitHub login when one is linked, and the git author name otherwise.
	Author   string
	AuthorAt string
	// HTMLURL is the commit's permalink, the pointer an operator follows from a
	// finding to the change itself.
	HTMLURL string
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

// Repositories lists what the installation selected, one bounded page from page
// (one-based), by stable IDs.
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
		HTMLURL string `json:"html_url"`
	}
	header, err := c.repoRead(ctx, token, query.RepositoryID, "/commits", parameters, &decoded)
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
			HTMLURL:  one.HTMLURL,
		}
		if one.Author != nil && one.Author.Login != "" {
			commit.Author = one.Author.Login
		}
		read.Commits = append(read.Commits, commit)
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

// maxResolvePages bounds the repository-name resolution walk: ten pages of one hundred
// cover any installation an investigation plausibly reads.
const maxResolvePages = 10

// repoRead performs one JSON read on a repository addressed by its stable id, over the
// documented /repos/{owner}/{repo} routes. The undocumented /repositories/{id} forms sit
// outside GitHub's versioning guarantees and are off this client's map.
func (c *Client) repoRead(
	ctx context.Context, token string, repository int64, suffix string,
	parameters url.Values, out any,
) (http.Header, error) {
	return c.repoReadBounded(ctx, token, repository, suffix, parameters, 0, out)
}

// repoReadBounded is repoRead under a caller-named response bound; zero means the
// standard one.
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

// withRepoName runs one read against the repository's resolved owner/name — and once
// more with a freshly resolved name when the answer looks like a rename (a 404, or a
// 301 left unfollowed): the id is still right, the cached name is not.
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

// fullName resolves a repository's owner/name from its stable id, from the
// installation's own repository listing, cached until a read proves it stale.
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
	// The bounded, named answer to "the customer did not select this repository". It is a
	// sentinel rather than a vendor refusal because it is not one: GitHub was never asked
	// about the repository, and nothing about it is retryable. There is no second
	// credential to fall back to and no second attempt to make — the fix is a person
	// adding the repository to the installation.
	return "", fmt.Errorf("%w (repository %d)", ErrRepositoryNotSelected, repository)
}

// call performs one JSON REST operation over rawCall and decodes the answer into out,
// returning the response headers for the callers that read pagination from them.
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

// fetchSpec is one read's shape: the standard JSON call leaves accept, rangeHeader and
// bound zero; the raw paths — file contents, log downloads — name their media type,
// their tail range, and the larger bound the responses documented to outgrow the
// standard one are given.
type fetchSpec struct {
	method      string
	path        string
	parameters  url.Values
	accept      string
	rangeHeader string
	bound       int
}

// fetch performs one HTTP attempt with this client's one set of headers, bounds and
// refusal mappings. A non-zero wait reports a retryable rate refusal.
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
	// The log download 302s to an expiring storage URL on another host; Go follows and
	// drops Authorization across hosts, which is exactly the behavior the redirect
	// needs — the storage URL carries its own authorization.
	request.Header.Set("Authorization", "Bearer "+credential)
	accept := spec.accept
	if accept == "" {
		accept = "application/vnd.github+json"
	}
	request.Header.Set("Accept", accept)
	if spec.rangeHeader != "" {
		request.Header.Set("Range", spec.rangeHeader)
	}
	// One pinned version, no negotiation: 2022-11-28 is supported on GitHub.com until
	// 2028-03-10 and is the version the widest range of GHES releases accepts. A 410
	// below is the cliff arriving, mapped to its own error so it self-diagnoses.
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	response, err := c.http.Do(request)
	if err != nil {
		// The transport error is wrapped, not quoted onward to an operator: url.Error
		// carries the full URL, and the caller decides what an operator sees.
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
			// The hourly budget: no named wait, a reset up to an hour away and not
			// worth sleeping towards.
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

// rawCall performs one read, retrying a rate refusal that names a wait exactly once,
// and only when the wait fits the caller's own deadline; an exhausted hourly budget is
// answered as ErrRateLimited immediately, because its reset is not worth sleeping
// towards. Every operation this client makes is a read or an idempotent mint, so the
// retry cannot double an effect.
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

// partialOmitsHead reports whether a ranged answer left out the representation's start.
// A suffix range covering a file whole still answers 206 with a range starting at zero
// — that is the whole content, not a truncation, and flagging it would over-claim. A
// partial answer that will not say its range is treated as truncated.
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
