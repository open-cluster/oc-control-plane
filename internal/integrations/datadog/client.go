// Package datadog is the Datadog provider: the Integration Type definition, live
// credential verification, and the read-only bounded tools an investigation reads
// monitors through.
//
// The vendor's payload shapes exist inside this package and nowhere else; what leaves is
// this system's own types. Everything read here is monitor state from a customer's
// account: it may be attacker-influenced and must never become an instruction, a
// destination, or an authorisation claim downstream.
package datadog

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

// maxResponseBytes bounds what one answer may hold. An answer that reaches this is not
// the API this client speaks.
const maxResponseBytes = 4 << 20

// requestTimeout is a backstop on one call. Every caller passes a bounded context; this
// exists so a caller that forgot cannot hold a connection forever.
const requestTimeout = 60 * time.Second

// ErrRateLimited reports that Datadog refused the call for rate. The read is safe to
// repeat later: everything this client does is a read, so no retry can double an effect.
var ErrRateLimited = errors.New("datadog is rate limiting this account's keys")

// APIError is Datadog's own refusal, decoded from its {"errors":[...]} envelope.
type APIError struct {
	Status int
	Detail string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("datadog answered %d: %s", e.Status, e.Detail)
}

// Client is the one HTTP client this provider holds. One per vendor, deliberately: a
// second client is a second place a header, a bound or a retry rule could differ.
//
// Unlike the other providers here, the origin is chosen PER CALL rather than once at
// construction: two Integrations of this type may name different Datadog sites (US, EU,
// a specific region), because the site is where a customer's own account lives, not a
// deployment-wide choice. override exists only for tests, which stand a fake at one fixed
// address regardless of the site argument a call names.
type Client struct {
	override string
	http     *http.Client
}

// NewClient builds the client. A non-empty override sends every call there regardless of
// the site a caller names, which is what lets a test stand a fake Datadog once.
func NewClient(override string) *Client {
	return &Client{override: strings.TrimSuffix(override, "/"), http: &http.Client{Timeout: requestTimeout}}
}

func (c *Client) origin(site string) string {
	if c.override != "" {
		return c.override
	}
	return "https://api." + site
}

// Monitor is one monitor as list and get report it.
type Monitor struct {
	ID           int64
	Name         string
	Type         string
	Query        string
	Message      string
	Tags         []string
	OverallState string
	Created      string
	Modified     string
}

// Monitors is a bounded page of an account's monitors.
type Monitors struct {
	Monitors []Monitor
	// Truncated reports that this page came back full, which on an unpaginated-total API
	// is the only signal available that more may exist: a short page proves there is no
	// more, a full one does not prove there is.
	Truncated bool
}

// MonitorsQuery bounds one listing.
type MonitorsQuery struct {
	// Name filters by a substring of the monitor's name.
	Name string
	// Tags is Datadog's own comma-separated monitor_tags filter.
	Tags  string
	Limit int
}

// Monitors lists the account's monitors, one bounded page.
func (c *Client) Monitors(ctx context.Context, site string, cred credential, query MonitorsQuery) (Monitors, error) {
	parameters := url.Values{"page_size": {strconv.Itoa(query.Limit)}}
	if query.Name != "" {
		parameters.Set("name", query.Name)
	}
	if query.Tags != "" {
		parameters.Set("monitor_tags", query.Tags)
	}

	var decoded []struct {
		ID           int64    `json:"id"`
		Name         string   `json:"name"`
		Type         string   `json:"type"`
		Query        string   `json:"query"`
		Message      string   `json:"message"`
		Tags         []string `json:"tags"`
		OverallState string   `json:"overall_state"`
		Created      string   `json:"created"`
		Modified     string   `json:"modified"`
	}
	if _, err := c.call(ctx, site, cred, "GET", "/api/v1/monitor", parameters, &decoded); err != nil {
		return Monitors{}, err
	}

	listed := Monitors{Monitors: make([]Monitor, 0, len(decoded)), Truncated: len(decoded) >= query.Limit}
	for _, one := range decoded {
		listed.Monitors = append(listed.Monitors, Monitor(one))
	}
	return listed, nil
}

// Monitor retrieves one monitor by id, whole.
func (c *Client) Monitor(ctx context.Context, site string, cred credential, id int64) (Monitor, error) {
	var decoded struct {
		ID           int64    `json:"id"`
		Name         string   `json:"name"`
		Type         string   `json:"type"`
		Query        string   `json:"query"`
		Message      string   `json:"message"`
		Tags         []string `json:"tags"`
		OverallState string   `json:"overall_state"`
		Created      string   `json:"created"`
		Modified     string   `json:"modified"`
	}
	if _, err := c.call(ctx, site, cred, "GET",
		"/api/v1/monitor/"+strconv.FormatInt(id, 10), nil, &decoded); err != nil {
		return Monitor{}, err
	}
	return Monitor(decoded), nil
}

// call performs one request and decodes its answer into out.
func (c *Client) call(
	ctx context.Context, site string, cred credential, method, path string, parameters url.Values, out any,
) (http.Header, error) {
	address := c.origin(site) + path
	if len(parameters) > 0 {
		address += "?" + parameters.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, method, address, nil)
	if err != nil {
		return nil, fmt.Errorf("building the %s request: %w", path, err)
	}
	request.Header.Set("DD-API-KEY", cred.APIKey)
	request.Header.Set("DD-APPLICATION-KEY", cred.ApplicationKey)
	request.Header.Set("Accept", "application/json")

	response, err := c.http.Do(request)
	if err != nil {
		// The transport error is wrapped, not quoted onward to an operator: url.Error
		// carries the full URL, and the caller decides what an operator sees.
		return nil, fmt.Errorf("reaching datadog for %s: %w", path, err)
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
			Errors []string `json:"errors"`
		}
		_ = json.Unmarshal(body, &refusal)
		detail := "unnamed error"
		if len(refusal.Errors) > 0 {
			detail = strings.Join(refusal.Errors, "; ")
		}
		return nil, &APIError{Status: response.StatusCode, Detail: detail}
	}

	if err := json.Unmarshal(body, out); err != nil {
		return nil, fmt.Errorf("decoding the %s answer: %w", path, err)
	}
	return response.Header, nil
}
