// Package pagerduty is the PagerDuty provider: the Integration Type definition, live
// token verification, and the read-only bounded tools an investigation reads incidents
// through.
//
// The vendor's payload shapes exist inside this package and nowhere else; what leaves is
// this system's own types. Everything read here is incident context from a customer's
// account: it may be attacker-influenced and must never become an instruction, a
// destination, or an authorisation claim downstream.
//
// The request and response shapes are taken from PagerDuty's own published OpenAPI
// document (github.com/PagerDuty/api-schema, reference/REST/openapiv3.json), not
// reconstructed from memory.
package pagerduty

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

// defaultBaseURL is the vendor's one API origin — unlike Datadog, Sentry and New Relic,
// PagerDuty has no per-customer region: every account is reached at the same address.
const defaultBaseURL = "https://api.pagerduty.com"

// apiVersion is the versioning header the vendor's Accept header carries. It is a request
// header rather than a URL segment, which is PagerDuty's own convention.
const apiVersion = "application/vnd.pagerduty+json;version=2"

// maxResponseBytes bounds what one answer may hold. An answer that reaches this is not
// the API this client speaks.
const maxResponseBytes = 4 << 20

// requestTimeout is a backstop on one call. Every caller passes a bounded context; this
// exists so a caller that forgot cannot hold a connection forever.
const requestTimeout = 60 * time.Second

// ErrRateLimited reports that PagerDuty refused the call for rate. The read is safe to
// repeat later: everything this client does is a read, so no retry can double an effect.
var ErrRateLimited = errors.New("pagerduty is rate limiting this account's token")

// APIError is PagerDuty's own refusal, decoded from its {"error":{"message","code"}}
// envelope.
type APIError struct {
	Status  int
	Code    int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("pagerduty answered %d: %s", e.Status, e.Message)
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
	return &Client{baseURL: strings.TrimSuffix(baseURL, "/"), http: &http.Client{Timeout: requestTimeout}}
}

// Incident is one incident as list and get report it.
type Incident struct {
	ID          string
	Number      int
	Title       string
	Status      string
	Urgency     string
	CreatedAt   string
	ServiceName string
	HTMLURL     string
}

// Incidents is a bounded page of an account's incidents.
type Incidents struct {
	Incidents []Incident
	// Truncated reports the vendor's own "more" flag: this page is not the whole answer.
	Truncated bool
}

// IncidentsQuery bounds one listing.
type IncidentsQuery struct {
	// Statuses is the vendor's own closed vocabulary: triggered, acknowledged, resolved.
	Statuses []string
	// Urgencies is the vendor's own closed vocabulary: high, low.
	Urgencies []string
	// Since and Until bound the window; empty means the vendor's own default (one month).
	Since, Until string
	Limit        int
}

// Incidents lists the account's incidents, one bounded page.
func (c *Client) Incidents(ctx context.Context, token string, query IncidentsQuery) (Incidents, error) {
	parameters := url.Values{"limit": {strconv.Itoa(query.Limit)}}
	for _, status := range query.Statuses {
		parameters.Add("statuses[]", status)
	}
	for _, urgency := range query.Urgencies {
		parameters.Add("urgencies[]", urgency)
	}
	if query.Since != "" {
		parameters.Set("since", query.Since)
	}
	if query.Until != "" {
		parameters.Set("until", query.Until)
	}

	var decoded struct {
		Incidents []incidentPayload `json:"incidents"`
		More      bool              `json:"more"`
	}
	if _, err := c.call(ctx, token, "GET", "/incidents", parameters, &decoded); err != nil {
		return Incidents{}, err
	}

	listed := Incidents{Incidents: make([]Incident, 0, len(decoded.Incidents)), Truncated: decoded.More}
	for _, one := range decoded.Incidents {
		listed.Incidents = append(listed.Incidents, one.toIncident())
	}
	return listed, nil
}

// Incident retrieves one incident by id or incident number.
func (c *Client) Incident(ctx context.Context, token, id string) (Incident, error) {
	var decoded struct {
		Incident incidentPayload `json:"incident"`
	}
	if _, err := c.call(ctx, token, "GET", "/incidents/"+url.PathEscape(id), nil, &decoded); err != nil {
		return Incident{}, err
	}
	return decoded.Incident.toIncident(), nil
}

// incidentPayload is the vendor's own shape; toIncident flattens the nested service
// reference into what this provider reports.
type incidentPayload struct {
	ID             string `json:"id"`
	IncidentNumber int    `json:"incident_number"`
	Title          string `json:"title"`
	Status         string `json:"status"`
	Urgency        string `json:"urgency"`
	CreatedAt      string `json:"created_at"`
	HTMLURL        string `json:"html_url"`
	Service        struct {
		Summary string `json:"summary"`
	} `json:"service"`
}

func (p incidentPayload) toIncident() Incident {
	return Incident{
		ID: p.ID, Number: p.IncidentNumber, Title: p.Title, Status: p.Status,
		Urgency: p.Urgency, CreatedAt: p.CreatedAt, ServiceName: p.Service.Summary, HTMLURL: p.HTMLURL,
	}
}

// call performs one request and decodes its answer into out.
func (c *Client) call(
	ctx context.Context, token, method, path string, parameters url.Values, out any,
) (http.Header, error) {
	address := c.baseURL + path
	if len(parameters) > 0 {
		address += "?" + parameters.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, method, address, nil)
	if err != nil {
		return nil, fmt.Errorf("building the %s request: %w", path, err)
	}
	request.Header.Set("Authorization", "Token token="+token)
	request.Header.Set("Accept", apiVersion)
	request.Header.Set("Content-Type", "application/json")

	response, err := c.http.Do(request)
	if err != nil {
		// The transport error is wrapped, not quoted onward to an operator: url.Error
		// carries the full URL, and the caller decides what an operator sees.
		return nil, fmt.Errorf("reaching pagerduty for %s: %w", path, err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode == http.StatusTooManyRequests {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes))
		return nil, ErrRateLimited
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading the %s answer: %w", path, err)
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("the %s answer exceeds %d bytes; refusing to read further",
			path, maxResponseBytes)
	}

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var refusal struct {
			Error struct {
				Message string `json:"message"`
				Code    int    `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &refusal)
		message := refusal.Error.Message
		if message == "" {
			message = "unnamed error"
		}
		return nil, &APIError{Status: response.StatusCode, Code: refusal.Error.Code, Message: message}
	}

	if err := json.Unmarshal(body, out); err != nil {
		return nil, fmt.Errorf("decoding the %s answer: %w", path, err)
	}
	return response.Header, nil
}
