// Package newrelic is the New Relic provider: the Integration Type definition, live user
// key verification, and the read-only bounded tools an investigation reads issues through.
//
// The vendor's payload shapes exist inside this package and nowhere else; what leaves is
// this system's own types. Everything read here is alert and event context from a
// customer's account: it may be attacker-influenced and must never become an instruction,
// a destination, or an authorisation claim downstream.
//
// This provider speaks NerdGraph, New Relic's GraphQL API, rather than a REST surface —
// the other providers in this package all speak REST, so the request shape here is the
// odd one out by the vendor's own design, not this codebase's.
package newrelic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// origins maps a region to its NerdGraph endpoint. A read against the wrong one answers
// exactly like a bad key — NerdGraph does not redirect between regions — so the region is
// closed to this set rather than typed as free text.
var origins = map[string]string{
	"us": "https://api.newrelic.com/graphql",
	"eu": "https://api.eu.newrelic.com/graphql",
	"jp": "https://api.jp.newrelic.com/graphql",
}

// Regions is the closed set, in the order offered.
var Regions = []string{"us", "eu", "jp"}

// maxResponseBytes bounds what one answer may hold. An answer that reaches this is not
// the API this client speaks.
const maxResponseBytes = 4 << 20

// requestTimeout is a backstop on one call. Every caller passes a bounded context; this
// exists so a caller that forgot cannot hold a connection forever.
const requestTimeout = 60 * time.Second

// experimentalOptIn is required by the vendor for every aiIssues call. New Relic's own
// documentation states the field is experimental and may change without notice; this
// provider accepts that because Issues is the read an incident investigation needs, and
// there is no stable alternative that answers the same question.
const (
	experimentalOptInHeader = "nerd-graph-unsafe-experimental-opt-in"
	experimentalOptInValue  = "AiIssues"
)

// ErrRateLimited reports that New Relic refused the call for rate. The read is safe to
// repeat later: everything this client does is a read, so no retry can double an effect.
var ErrRateLimited = errors.New("new relic is rate limiting this account's key")

// APIError is New Relic's own refusal, whether carried as an HTTP status or as NerdGraph's
// own "errors" array inside a 200.
type APIError struct {
	Status int
	Detail string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("new relic answered %d: %s", e.Status, e.Detail)
}

// Client is the one HTTP client this provider holds. One per vendor, deliberately: a
// second client is a second place a header, a bound or a retry rule could differ.
//
// The origin is chosen PER CALL, like the Datadog provider's site: two Integrations of
// this type may name different regions, because the region is where a customer's own
// account lives. override exists only for tests.
type Client struct {
	override string
	http     *http.Client
}

// NewClient builds the client. A non-empty override sends every call there regardless of
// the region a caller names, which is what lets a test stand a fake NerdGraph once.
func NewClient(override string) *Client {
	return &Client{override: strings.TrimSuffix(override, "/"), http: &http.Client{Timeout: requestTimeout}}
}

func (c *Client) origin(region string) string {
	if c.override != "" {
		return c.override
	}
	return origins[region]
}

// Issue is one issue as list and get report it.
type Issue struct {
	ID             string
	Title          string
	Priority       string
	State          string
	EntityNames    []string
	Description    string
	ActivatedAt    int64
	AcknowledgedAt int64
	ClosedAt       int64
}

// Issues is a bounded page of an account's issues.
type Issues struct {
	Issues []Issue
	// Truncated reports that the vendor's own cursor pagination holds a next page.
	Truncated bool
}

// IssuesQuery bounds one listing.
type IssuesQuery struct {
	// Priorities filters by New Relic's own priority vocabulary: LOW, MEDIUM, HIGH, CRITICAL.
	Priorities []string
	// States filters by New Relic's own state vocabulary: ACTIVATED, ACKNOWLEDGED, CLOSED.
	States []string
	Limit  int
}

const listIssuesQuery = `
query($accountId: Int!, $priorities: [AiIssuesPriority], $states: [AiIssuesIncidentState], $cursor: String) {
  actor {
    account(id: $accountId) {
      aiIssues {
        issues(filter: {priority: $priorities, states: $states}, cursor: $cursor) {
          issues {
            issueId
            title
            priority
            state
            entityNames
            description
            activatedAt
            acknowledgedAt
            closedAt
          }
          nextCursor
        }
      }
    }
  }
}`

const getIssueQuery = `
query($accountId: Int!, $issueId: ID!) {
  actor {
    account(id: $accountId) {
      aiIssues {
        issues(filter: {ids: [$issueId]}) {
          issues {
            issueId
            title
            priority
            state
            entityNames
            description
            activatedAt
            acknowledgedAt
            closedAt
          }
        }
      }
    }
  }
}`

// issuesEnvelope is the shape both queries decode into; get asks for one id and reads the
// first element the vendor returns.
type issuesEnvelope struct {
	Actor struct {
		Account struct {
			AiIssues struct {
				Issues struct {
					Issues []struct {
						IssueID        string   `json:"issueId"`
						Title          string   `json:"title"`
						Priority       string   `json:"priority"`
						State          string   `json:"state"`
						EntityNames    []string `json:"entityNames"`
						Description    string   `json:"description"`
						ActivatedAt    int64    `json:"activatedAt"`
						AcknowledgedAt int64    `json:"acknowledgedAt"`
						ClosedAt       int64    `json:"closedAt"`
					} `json:"issues"`
					NextCursor *string `json:"nextCursor"`
				} `json:"issues"`
			} `json:"aiIssues"`
		} `json:"account"`
	} `json:"actor"`
}

// Issues lists an account's issues, one bounded page.
func (c *Client) Issues(ctx context.Context, region, key string, accountID int, query IssuesQuery) (Issues, error) {
	var envelope issuesEnvelope
	if err := c.call(ctx, region, key, listIssuesQuery, map[string]any{
		"accountId":  accountID,
		"priorities": query.Priorities,
		"states":     query.States,
		"cursor":     nil,
	}, &envelope); err != nil {
		return Issues{}, err
	}

	found := envelope.Actor.Account.AiIssues.Issues
	listed := Issues{
		Issues:    make([]Issue, 0, len(found.Issues)),
		Truncated: found.NextCursor != nil && *found.NextCursor != "",
	}
	for i, one := range found.Issues {
		if i >= query.Limit {
			listed.Truncated = true
			break
		}
		listed.Issues = append(listed.Issues, Issue{
			ID: one.IssueID, Title: one.Title, Priority: one.Priority, State: one.State,
			EntityNames: one.EntityNames, Description: one.Description,
			ActivatedAt: one.ActivatedAt, AcknowledgedAt: one.AcknowledgedAt, ClosedAt: one.ClosedAt,
		})
	}
	return listed, nil
}

// Issue retrieves one issue by id.
func (c *Client) Issue(ctx context.Context, region, key string, accountID int, issueID string) (Issue, error) {
	var envelope issuesEnvelope
	if err := c.call(ctx, region, key, getIssueQuery, map[string]any{
		"accountId": accountID,
		"issueId":   issueID,
	}, &envelope); err != nil {
		return Issue{}, err
	}

	found := envelope.Actor.Account.AiIssues.Issues.Issues
	if len(found) == 0 {
		return Issue{}, &APIError{Status: http.StatusNotFound, Detail: "no issue with that id"}
	}
	one := found[0]
	return Issue{
		ID: one.IssueID, Title: one.Title, Priority: one.Priority, State: one.State,
		EntityNames: one.EntityNames, Description: one.Description,
		ActivatedAt: one.ActivatedAt, AcknowledgedAt: one.AcknowledgedAt, ClosedAt: one.ClosedAt,
	}, nil
}

// call performs one GraphQL request and decodes its "data" into out.
func (c *Client) call(
	ctx context.Context, region, key, query string, variables map[string]any, out any,
) error {
	origin := c.origin(region)
	if origin == "" {
		return fmt.Errorf("%q is not a region this provider knows", region)
	}

	body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return fmt.Errorf("encoding the graphql request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, origin, strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("building the request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("API-Key", key)
	request.Header.Set(experimentalOptInHeader, experimentalOptInValue)

	response, err := c.http.Do(request)
	if err != nil {
		// The transport error is wrapped, not quoted onward to an operator: url.Error
		// carries the full URL, and the caller decides what an operator sees.
		return fmt.Errorf("reaching new relic: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode == http.StatusTooManyRequests {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes))
		return ErrRateLimited
	}

	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("reading the answer: %w", err)
	}
	if len(raw) > maxResponseBytes {
		return fmt.Errorf("the answer exceeds %d bytes; refusing to read further", maxResponseBytes)
	}

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &APIError{Status: response.StatusCode, Detail: strings.TrimSpace(string(raw))}
	}

	// NerdGraph answers 200 even for a query it refused; the refusal travels in its own
	// "errors" array beside "data", which every GraphQL server uses for exactly this.
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("the answer is not graphql json: %w", err)
	}
	if len(envelope.Errors) > 0 {
		messages := make([]string, 0, len(envelope.Errors))
		for _, one := range envelope.Errors {
			messages = append(messages, one.Message)
		}
		return &APIError{Status: response.StatusCode, Detail: strings.Join(messages, "; ")}
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("decoding the answer: %w", err)
	}
	return nil
}

// accountIDOf parses a configuration value into the whole positive number NerdGraph's
// Int! account id argument needs.
func accountIDOf(value any) (int, error) {
	number, isNumber := value.(float64)
	if !isNumber || number != float64(int(number)) || number <= 0 {
		return 0, errors.New("accountId is not a whole positive number")
	}
	return int(number), nil
}
