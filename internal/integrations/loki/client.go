package loki

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxResponseBytes = 2 << 20

var errRedirect = errors.New("loki redirects are not allowed; configure the final URL")

var httpClient = &http.Client{
	Timeout:       10 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return errRedirect },
}

type client struct {
	endpoint                           *url.URL
	credential, tenant, auth, username string
}

func newClient(configuration map[string]any, credential string) (*client, error) {
	address, _ := configuration["url"].(string)
	endpoint, err := url.Parse(address)
	if err != nil || len(address) > 2048 || (endpoint.Scheme != "http" && endpoint.Scheme != "https") ||
		endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery ||
		endpoint.Fragment != "" || endpoint.RawPath != "" || endpoint.Opaque != "" {
		return nil, errors.New("loki URL must be absolute HTTP(S) without credentials, query, fragment or escaped path")
	}
	for _, part := range strings.Split(endpoint.Path, "/") {
		if part == "." || part == ".." {
			return nil, errors.New("loki URL must not contain path traversal")
		}
	}
	tenant, _ := configuration["tenantId"].(string)
	if len(tenant) > 150 {
		return nil, errors.New("loki tenantId must contain at most 150 ASCII characters")
	}
	for _, character := range tenant {
		if character < '!' || character > '~' || character == '|' {
			return nil, errors.New("loki tenantId must identify one tenant without whitespace or control characters")
		}
	}
	if strings.ContainsAny(credential, "\r\n") {
		return nil, errors.New("loki credential contains an invalid line break")
	}
	auth, _ := configuration["auth"].(string)
	username, _ := configuration["username"].(string)
	switch auth {
	case "bearer":
		if username != "" {
			return nil, errors.New("loki username is only supported with Basic authentication")
		}
	case "basic":
		if username == "" || len(username) > 256 || strings.ContainsAny(username, ":\r\n") || credential == "" {
			return nil, errors.New("loki Basic authentication requires a valid username and token")
		}
	default:
		return nil, errors.New("loki auth must explicitly select bearer or basic")
	}
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/loki/api/v1/query_range"
	return &client{endpoint: endpoint, credential: credential, tenant: tenant, auth: auth, username: username}, nil
}

type entry struct {
	Timestamp time.Time         `json:"timestamp"`
	Line      string            `json:"line"`
	Labels    map[string]string `json:"labels"`
}

func (c *client) query(ctx context.Context, query string, from, until time.Time, limit int, direction string) ([]entry, error) {
	endpoint := *c.endpoint
	endpoint.RawQuery = url.Values{
		"query": {query}, "start": {strconv.FormatInt(from.UnixNano(), 10)},
		"end": {strconv.FormatInt(until.UnixNano(), 10)}, "limit": {strconv.Itoa(limit)}, "direction": {direction},
	}.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, errors.New("loki request could not be configured")
	}
	request.Header.Set("Accept", "application/json")
	if c.auth == "basic" {
		request.SetBasicAuth(c.username, c.credential)
	} else if c.credential != "" {
		request.Header.Set("Authorization", "Bearer "+c.credential)
	}
	if c.tenant != "" {
		request.Header.Set("X-Scope-OrgID", c.tenant)
	}
	response, err := httpClient.Do(request)
	if err != nil {
		var networkError net.Error
		switch {
		case errors.Is(err, errRedirect):
			return nil, errRedirect
		case errors.Is(err, context.Canceled):
			return nil, errors.New("loki request cancelled")
		case errors.Is(err, context.DeadlineExceeded), errors.As(err, &networkError) && networkError.Timeout():
			return nil, errors.New("loki request timed out")
		default:
			return nil, errors.New("loki source is unavailable")
		}
	}
	defer func() { _ = response.Body.Close() }()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, errors.New("loki authentication or query permission denied")
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return nil, errors.New("loki rejected the query or parameters")
	case http.StatusTooManyRequests:
		return nil, errors.New("loki query rate limit exceeded")
	default:
		return nil, errors.New("loki source is unavailable or timed out")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, errors.New("loki response could not be read")
	}
	if len(body) > maxResponseBytes {
		return nil, errors.New("loki response exceeds the safe size limit; narrow the query")
	}
	var decoded struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Stream map[string]string   `json:"stream"`
				Values [][]json.RawMessage `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &decoded) != nil || decoded.Status != "success" || decoded.Data.ResultType != "streams" || decoded.Data.Result == nil {
		return nil, errors.New("loki returned a malformed or unsupported log result")
	}
	entries := make([]entry, 0)
	for _, stream := range decoded.Data.Result {
		if stream.Stream == nil || stream.Values == nil {
			return nil, errors.New("loki returned a malformed log stream")
		}
		for _, pair := range stream.Values {
			var timestamp, line *string
			if len(pair) < 2 || len(pair) > 3 || json.Unmarshal(pair[0], &timestamp) != nil ||
				json.Unmarshal(pair[1], &line) != nil || timestamp == nil || line == nil {
				return nil, errors.New("loki returned a malformed log entry")
			}
			nanoseconds, err := strconv.ParseInt(*timestamp, 10, 64)
			at := time.Unix(0, nanoseconds).UTC()
			if err != nil || at.Before(from) || !at.Before(until) {
				return nil, errors.New("loki returned an invalid or out-of-window log timestamp")
			}
			entries = append(entries, entry{Timestamp: at, Line: *line, Labels: stream.Stream})
		}
	}
	return entries, nil
}
