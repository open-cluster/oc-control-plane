// Package sentry is the Sentry provider: the Integration Type definition, live token
// verification, and the read-only bounded tools an investigation reads issues through.
//
// The vendor's payload shapes exist inside this package and nowhere else; what leaves is
// this system's own types. Everything read here is error and event context from a
// customer's projects: it may be attacker-influenced and must never become an
// instruction, a destination, or an authorisation claim downstream.
package sentry

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

// defaultBaseURL is where the SaaS API lives. Self-hosted Sentry serves the same path
// shape from its own origin, which is why the base URL is configuration rather than a
// constant baked into every call.
const defaultBaseURL = "https://sentry.io/api/0"

// maxResponseBytes bounds what one answer may hold. An answer that reaches this is not
// the API this client speaks.
const maxResponseBytes = 4 << 20

// requestTimeout is a backstop on one call. Every caller passes a bounded context; this
// exists so a caller that forgot cannot hold a connection forever.
const requestTimeout = 60 * time.Second

// maxRetryWait bounds how long a rate-limit reset is worth waiting for. A vendor asking
// for more is answered as rate-limited now: one bounded read must not park a goroutine on
// the vendor's say-so, deadline or none.
const maxRetryWait = 30 * time.Second

// ErrRateLimited reports that Sentry answered 429 twice, or asked for a wait the caller's
// own deadline cannot hold. The read is safe to repeat later: everything this client does
// is a read, so no retry can double an effect.
var ErrRateLimited = errors.New("sentry is rate limiting this organization's token")

// APIError is Sentry's own refusal. It is typed so verification can tell a revoked token
// from a missing scope from an unreachable vendor — three different answers to an
// operator.
type APIError struct {
	Status int
	Detail string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("sentry answered %d: %s", e.Status, e.Detail)
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

// Organization is who a token belongs to, from retrieving the organization directly —
// the identity check behind "verified means the far end answered".
type Organization struct {
	ID   string
	Slug string
	Name string
}

// Issue is one issue as list and get report it.
//
// Count and UserCount are decoded through flexNumber: Sentry's own documentation does not
// pin down whether they arrive as JSON strings or numbers, and a wrong guess would fail
// decoding the whole issue rather than degrade one field.
type Issue struct {
	ID        string
	ShortID   string
	Title     string
	Culprit   string
	Level     string
	Status    string
	Count     string
	UserCount string
	FirstSeen time.Time
	LastSeen  time.Time
	Permalink string
	Project   string
}

// Issues is a bounded page of an organization's issues.
type Issues struct {
	Issues []Issue
	// Truncated reports that the vendor's own pagination holds a next page; a reader must
	// not mistake this page for the whole set.
	Truncated bool
}

// IssuesQuery bounds one listing.
type IssuesQuery struct {
	// Query is Sentry's own search syntax: "is:unresolved", "level:error", a free-text
	// term. Empty returns the organization's default view.
	Query string
	// StatsPeriod is the vendor's relative window, such as "24h" or "14d". Empty is the
	// vendor's own default.
	StatsPeriod string
	Limit       int
}

// Organization retrieves the organization directly, which is the probe behind "verified
// means the far end answered as this organization".
func (c *Client) Organization(ctx context.Context, token, orgSlug string) (Organization, error) {
	var decoded struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
		Name string `json:"name"`
	}
	if _, err := c.call(ctx, token, "GET",
		"/organizations/"+url.PathEscape(orgSlug)+"/", nil, &decoded); err != nil {
		return Organization{}, err
	}
	return Organization(decoded), nil
}

// Issues lists an organization's issues, one bounded page, in the order the vendor
// returns them.
func (c *Client) Issues(ctx context.Context, token, orgSlug string, query IssuesQuery) (Issues, error) {
	parameters := url.Values{"limit": {strconv.Itoa(query.Limit)}}
	if query.Query != "" {
		parameters.Set("query", query.Query)
	}
	if query.StatsPeriod != "" {
		parameters.Set("statsPeriod", query.StatsPeriod)
	}

	var decoded []struct {
		ID        string     `json:"id"`
		ShortID   string     `json:"shortId"`
		Title     string     `json:"title"`
		Culprit   string     `json:"culprit"`
		Level     string     `json:"level"`
		Status    string     `json:"status"`
		Count     flexNumber `json:"count"`
		UserCount flexNumber `json:"userCount"`
		FirstSeen time.Time  `json:"firstSeen"`
		LastSeen  time.Time  `json:"lastSeen"`
		Permalink string     `json:"permalink"`
		Project   struct {
			Slug string `json:"slug"`
		} `json:"project"`
	}
	header, err := c.call(ctx, token, "GET",
		"/organizations/"+url.PathEscape(orgSlug)+"/issues/", parameters, &decoded)
	if err != nil {
		return Issues{}, err
	}

	listed := Issues{Issues: make([]Issue, 0, len(decoded)), Truncated: nextPageLinked(header)}
	for _, one := range decoded {
		listed.Issues = append(listed.Issues, Issue{
			ID: one.ID, ShortID: one.ShortID, Title: one.Title, Culprit: one.Culprit,
			Level: one.Level, Status: one.Status, Count: string(one.Count),
			UserCount: string(one.UserCount), FirstSeen: one.FirstSeen, LastSeen: one.LastSeen,
			Permalink: one.Permalink, Project: one.Project.Slug,
		})
	}
	return listed, nil
}

// Issue retrieves one issue by id, whole.
func (c *Client) Issue(ctx context.Context, token, orgSlug, issueID string) (Issue, error) {
	var decoded struct {
		ID        string     `json:"id"`
		ShortID   string     `json:"shortId"`
		Title     string     `json:"title"`
		Culprit   string     `json:"culprit"`
		Level     string     `json:"level"`
		Status    string     `json:"status"`
		Count     flexNumber `json:"count"`
		UserCount flexNumber `json:"userCount"`
		FirstSeen time.Time  `json:"firstSeen"`
		LastSeen  time.Time  `json:"lastSeen"`
		Permalink string     `json:"permalink"`
		Project   struct {
			Slug string `json:"slug"`
		} `json:"project"`
	}
	if _, err := c.call(ctx, token, "GET",
		"/organizations/"+url.PathEscape(orgSlug)+"/issues/"+url.PathEscape(issueID)+"/",
		nil, &decoded); err != nil {
		return Issue{}, err
	}
	return Issue{
		ID: decoded.ID, ShortID: decoded.ShortID, Title: decoded.Title, Culprit: decoded.Culprit,
		Level: decoded.Level, Status: decoded.Status, Count: string(decoded.Count),
		UserCount: string(decoded.UserCount), FirstSeen: decoded.FirstSeen, LastSeen: decoded.LastSeen,
		Permalink: decoded.Permalink, Project: decoded.Project.Slug,
	}, nil
}

// call performs one request and decodes its answer into out, returning the response
// headers for the one caller that reads pagination from them.
//
// A 429 is retried exactly once, after the reset the vendor reported, and only when that
// wait fits the caller's own deadline. Every method this client speaks is a read, so the
// retry cannot double an effect; a second 429 is answered as ErrRateLimited rather than by
// queueing behind a vendor that has said no twice.
func (c *Client) call(
	ctx context.Context, token, method, path string, parameters url.Values, out any,
) (http.Header, error) {
	for attempt := 0; ; attempt++ {
		header, wait, err := c.once(ctx, token, method, path, parameters, out)
		if wait == 0 || attempt == 1 {
			if wait != 0 {
				return nil, fmt.Errorf("%w: %s answered 429 twice", ErrRateLimited, path)
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

// once performs one attempt. A non-zero wait reports a 429 and how long the vendor asked
// for; everything else is the final answer.
func (c *Client) once(
	ctx context.Context, token, method, path string, parameters url.Values, out any,
) (http.Header, time.Duration, error) {
	address := c.baseURL + path
	if len(parameters) > 0 {
		address += "?" + parameters.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, method, address, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("building the %s request: %w", path, err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")

	response, err := c.http.Do(request)
	if err != nil {
		// The transport error is wrapped, not quoted onward to an operator: url.Error
		// carries the full URL, and the caller decides what an operator sees.
		return nil, 0, fmt.Errorf("reaching sentry for %s: %w", path, err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode == http.StatusTooManyRequests {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes))
		return nil, rateLimitReset(response.Header), nil
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, 0, fmt.Errorf("reading the %s answer: %w", path, err)
	}
	if len(body) > maxResponseBytes {
		return nil, 0, fmt.Errorf("the %s answer exceeds %d bytes; refusing to read further",
			path, maxResponseBytes)
	}

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var refusal struct {
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(body, &refusal)
		if refusal.Detail == "" {
			refusal.Detail = "unnamed error"
		}
		return nil, 0, &APIError{Status: response.StatusCode, Detail: refusal.Detail}
	}

	if err := json.Unmarshal(body, out); err != nil {
		return nil, 0, fmt.Errorf("decoding the %s answer: %w", path, err)
	}
	return response.Header, 0, nil
}

// rateLimitReset reads how long until the vendor's own window resets, with a short
// default where the header is missing or unreadable.
func rateLimitReset(header http.Header) time.Duration {
	epoch, err := strconv.ParseInt(strings.TrimSpace(header.Get("X-Sentry-Rate-Limit-Reset")), 10, 64)
	if err != nil {
		return time.Second
	}
	wait := time.Until(time.Unix(epoch, 0))
	if wait < time.Second {
		return time.Second
	}
	return wait
}

// nextPageLinked reports whether the Link header names a next page with results, in the
// cursor-pagination shape every listing answers with.
func nextPageLinked(header http.Header) bool {
	for _, part := range strings.Split(header.Get("Link"), ",") {
		if strings.Contains(part, `rel="next"`) && strings.Contains(part, `results="true"`) {
			return true
		}
	}
	return false
}

// flexNumber decodes a JSON string or a JSON number into a Go string. Sentry's own
// documentation does not pin down which shape count and userCount arrive as, and a client
// that guessed wrong would fail decoding the whole issue rather than degrade one field.
type flexNumber string

func (n *flexNumber) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if len(trimmed) >= 2 && trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
		*n = flexNumber(text)
		return nil
	}
	*n = flexNumber(trimmed)
	return nil
}
