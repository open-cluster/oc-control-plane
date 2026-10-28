package tempo

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const maxResponseBytes = 2 << 20

var (
	errRedirect = errors.New("tempo redirects are not allowed; configure the final URL")
	errNotFound = errors.New("tempo trace or endpoint was not found in the searched window")
	httpClient  = &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return errRedirect },
	}
)

type client struct {
	endpoint              *url.URL
	tenant, authorization string
}

func newClient(configuration map[string]any, credential string) (*client, error) {
	address, _ := configuration["url"].(string)
	endpoint, err := url.Parse(address)
	if err != nil || len(address) > 2048 || (endpoint.Scheme != "http" && endpoint.Scheme != "https") ||
		endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery ||
		endpoint.Fragment != "" || endpoint.RawPath != "" || endpoint.Opaque != "" {
		return nil, errors.New("tempo URL must be absolute HTTP(S) without credentials, query, fragment or escaped path")
	}
	for _, part := range strings.Split(endpoint.Path, "/") {
		if part == "." || part == ".." {
			return nil, errors.New("tempo URL must not contain path traversal")
		}
	}
	tenant, _ := configuration["tenantId"].(string)
	if len(tenant) > 150 {
		return nil, errors.New("tempo tenantId must contain at most 150 ASCII characters")
	}
	for _, character := range tenant {
		if character < '!' || character > '~' || character == '|' {
			return nil, errors.New("tempo tenantId must identify one tenant without whitespace or control characters")
		}
	}
	if strings.ContainsAny(credential, "\r\n") {
		return nil, errors.New("tempo credential contains an invalid line break")
	}
	auth, _ := configuration["auth"].(string)
	username, _ := configuration["username"].(string)
	authorization := ""
	switch auth {
	case "bearer":
		if username != "" {
			return nil, errors.New("tempo username is only supported with Basic authentication")
		}
		if credential != "" {
			authorization = "Bearer " + credential
		}
	case "basic":
		if username == "" || len(username) > 256 || strings.ContainsAny(username, ":\r\n") || credential == "" {
			return nil, errors.New("tempo Basic authentication requires a valid username and token")
		}
		authorization = "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+credential))
	default:
		return nil, errors.New("tempo auth must explicitly select bearer or basic")
	}
	endpoint.Path = strings.TrimRight(endpoint.Path, "/")
	return &client{endpoint: endpoint, tenant: tenant, authorization: authorization}, nil
}

func (c *client) get(ctx context.Context, path string, parameters url.Values, into any) error {
	endpoint := *c.endpoint
	endpoint.Path += path
	endpoint.RawQuery = parameters.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return errors.New("tempo request could not be configured")
	}
	request.Header.Set("Accept", "application/json")
	if c.authorization != "" {
		request.Header.Set("Authorization", c.authorization)
	}
	if c.tenant != "" {
		request.Header.Set("X-Scope-OrgID", c.tenant)
	}
	response, err := httpClient.Do(request)
	if err != nil {
		var networkError net.Error
		switch {
		case errors.Is(err, errRedirect):
			return errRedirect
		case errors.Is(err, context.Canceled):
			return errors.New("tempo request cancelled")
		case errors.Is(err, context.DeadlineExceeded), errors.As(err, &networkError) && networkError.Timeout():
			return errors.New("tempo request timed out")
		default:
			return errors.New("tempo source is unavailable")
		}
	}
	defer func() { _ = response.Body.Close() }()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return errors.New("tempo authentication or query permission denied")
	case http.StatusNotFound:
		return errNotFound
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return errors.New("tempo rejected the query, parameters or source limits")
	case http.StatusTooManyRequests:
		return errors.New("tempo query rate limit exceeded")
	default:
		return errors.New("tempo source is unavailable or timed out")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return errors.New("tempo response could not be read")
	}
	if len(body) > maxResponseBytes {
		return errors.New("tempo response exceeds the safe size limit; narrow the request")
	}
	if json.Unmarshal(body, into) != nil {
		return errors.New("tempo returned malformed or unsupported JSON")
	}
	return nil
}

func windowParameters(from, until time.Time) url.Values {
	return url.Values{"start": {strconv.FormatInt(from.Unix(), 10)}, "end": {strconv.FormatInt(until.Unix(), 10)}}
}

type searchResponse struct {
	Traces  []json.RawMessage                          `json:"traces"`
	Metrics *struct{ CompletedJobs, TotalJobs uint32 } `json:"metrics"`
}

func (c *client) search(ctx context.Context, query string, from, until time.Time, limit int) (searchResponse, error) {
	parameters := windowParameters(from, until)
	parameters.Set("q", query)
	parameters.Set("limit", strconv.Itoa(limit))
	var response searchResponse
	if err := c.get(ctx, "/api/search", parameters, &response); err != nil {
		return response, err
	}
	if response.Traces == nil && response.Metrics == nil {
		return response, errors.New("tempo returned a malformed search response")
	}
	return response, nil
}

type attribute struct {
	Key   string `json:"key"`
	Value struct {
		StringValue *string `json:"stringValue"`
	} `json:"value"`
}

type serviceStats struct {
	SpanCount  uint32 `json:"spanCount"`
	ErrorCount uint32 `json:"errorCount"`
}

type traceSummary struct {
	TraceID      string                   `json:"traceId"`
	RootService  string                   `json:"rootService"`
	Operation    string                   `json:"operation"`
	Start        time.Time                `json:"start"`
	DurationMs   *uint32                  `json:"durationMs"`
	ServiceStats map[string]*serviceStats `json:"serviceStats,omitempty"`
	ErrorHints   []string                 `json:"errorHints,omitempty"`
}

func searchSummaries(raw []json.RawMessage) ([]traceSummary, bool, error) {
	summaries := make([]traceSummary, 0, len(raw))
	limited := false
	for _, item := range raw {
		var metadata struct {
			TraceID, RootServiceName, RootTraceName string
			StartTimeUnixNano                       uint64                   `json:"startTimeUnixNano,string"`
			DurationMs                              *uint32                  `json:"durationMs"`
			ServiceStats                            map[string]*serviceStats `json:"serviceStats"`
			SpanSets                                []struct {
				Spans []struct{ Attributes []attribute }
			}
		}
		if json.Unmarshal(item, &metadata) != nil {
			return nil, false, errors.New("tempo returned malformed trace metadata")
		}
		id, err := normalizedID(metadata.TraceID, 32)
		if err != nil || metadata.StartTimeUnixNano == 0 || metadata.StartTimeUnixNano > math.MaxInt64 {
			return nil, false, errors.New("tempo returned an invalid trace identifier or timestamp")
		}
		summary := traceSummary{TraceID: id, RootService: metadata.RootServiceName, Operation: metadata.RootTraceName,
			Start: time.Unix(0, int64(metadata.StartTimeUnixNano)).UTC(), DurationMs: metadata.DurationMs, ServiceStats: metadata.ServiceStats}
		for _, set := range metadata.SpanSets {
			for _, span := range set.Spans {
				for _, attr := range span.Attributes {
					switch attr.Key {
					case "status", "statusMessage", "error.type", "exception.type", "exception.message":
						if attr.Value.StringValue != nil {
							if len(summary.ErrorHints) < 8 {
								summary.ErrorHints = append(summary.ErrorHints, attr.Key+"="+*attr.Value.StringValue)
							} else {
								limited = true
							}
						}
					}
				}
			}
		}
		summaries = append(summaries, summary)
	}
	return summaries, limited, nil
}

func normalizedID(id string, width int) (string, error) {
	if id == "" || len(id) > width || strings.Trim(id, "0") == "" {
		return "", errors.New("tempo identifier must be nonzero hexadecimal within the supported size")
	}
	id = strings.Repeat("0", width-len(id)) + strings.ToLower(id)
	if _, err := hex.DecodeString(id); err != nil {
		return "", errors.New("tempo identifier must be hexadecimal")
	}
	return id, nil
}

type span struct {
	ID            string    `json:"id"`
	ParentID      string    `json:"parentId,omitempty"`
	ParentMissing bool      `json:"parentMissing,omitempty"`
	Service       string    `json:"service"`
	Operation     string    `json:"operation"`
	Start         time.Time `json:"start"`
	End           time.Time `json:"end"`
	DurationNanos uint64    `json:"durationNanos"`
	Status        string    `json:"status"`
	StatusMessage string    `json:"statusMessage,omitempty"`
}

type traceView struct {
	TraceID        string `json:"traceId"`
	BackendStatus  string `json:"backendStatus,omitempty"`
	BackendMessage string `json:"backendMessage,omitempty"`
	Spans          []span `json:"spans"`
	Incomplete     bool   `json:"incomplete"`
}

type wireSpan struct {
	TraceID      []byte `json:"traceId"`
	SpanID       []byte `json:"spanId"`
	ParentSpanID []byte `json:"parentSpanId"`
	Name         string `json:"name"`
	Start        uint64 `json:"startTimeUnixNano,string"`
	End          uint64 `json:"endTimeUnixNano,string"`
	Status       struct {
		Code    json.RawMessage `json:"code"`
		Message string          `json:"message"`
	} `json:"status"`
}

func (c *client) trace(ctx context.Context, id string, from, until time.Time) (traceView, error) {
	view := traceView{TraceID: id, Spans: make([]span, 0)}
	parameters := windowParameters(from, until)
	parameters.Set("span_pruning", "false")
	var response struct {
		Trace *struct {
			ResourceSpans []struct {
				Resource   struct{ Attributes []attribute } `json:"resource"`
				ScopeSpans []struct {
					Spans []wireSpan `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		} `json:"trace"`
		Status  json.RawMessage `json:"status"`
		Message string          `json:"message"`
	}
	if err := c.get(ctx, "/api/v2/traces/"+id, parameters, &response); err != nil {
		return view, err
	}
	if response.Trace == nil || response.Trace.ResourceSpans == nil {
		return view, errors.New("tempo returned a malformed trace response")
	}
	switch string(response.Status) {
	case "", "0", `"COMPLETE"`:
		view.BackendStatus = "COMPLETE"
	case "1", `"PARTIAL"`:
		view.BackendStatus = "PARTIAL"
		view.Incomplete = true
	default:
		return view, errors.New("tempo returned an unsupported trace completeness status")
	}
	view.BackendMessage = response.Message
	for _, resource := range response.Trace.ResourceSpans {
		service := ""
		for _, attr := range resource.Resource.Attributes {
			if attr.Key == "service.name" && attr.Value.StringValue != nil {
				service = *attr.Value.StringValue
			}
		}
		if resource.ScopeSpans == nil {
			return view, errors.New("tempo returned a malformed span resource")
		}
		for _, scope := range resource.ScopeSpans {
			if scope.Spans == nil {
				return view, errors.New("tempo returned a malformed span scope")
			}
			for _, raw := range scope.Spans {
				traceID, traceErr := normalizedID(hex.EncodeToString(raw.TraceID), 32)
				spanID, spanErr := normalizedID(hex.EncodeToString(raw.SpanID), 16)
				parentID := ""
				var parentErr error
				if strings.Trim(hex.EncodeToString(raw.ParentSpanID), "0") != "" {
					parentID, parentErr = normalizedID(hex.EncodeToString(raw.ParentSpanID), 16)
				}
				if traceErr != nil || spanErr != nil || parentErr != nil || len(raw.ParentSpanID) > 8 || traceID != id || parentID == spanID {
					return view, errors.New("tempo returned an invalid or mismatched span identifier")
				}
				if raw.Start == 0 || raw.Start > math.MaxInt64 || raw.End < raw.Start || raw.End > math.MaxInt64 {
					return view, errors.New("tempo returned an invalid span timestamp or duration")
				}
				status := ""
				switch string(raw.Status.Code) {
				case "", "0", `"STATUS_CODE_UNSET"`:
					status = "unset"
				case "1", `"STATUS_CODE_OK"`:
					status = "ok"
				case "2", `"STATUS_CODE_ERROR"`:
					status = "error"
				default:
					return view, errors.New("tempo returned an unsupported span status")
				}
				view.Spans = append(view.Spans, span{ID: spanID, ParentID: parentID, Service: service, Operation: raw.Name,
					Start: time.Unix(0, int64(raw.Start)).UTC(), End: time.Unix(0, int64(raw.End)).UTC(), DurationNanos: raw.End - raw.Start, Status: status, StatusMessage: raw.Status.Message})
			}
		}
	}
	if len(view.Spans) == 0 {
		return view, errors.New("tempo returned a trace without spans")
	}
	sort.SliceStable(view.Spans, func(i, j int) bool { return view.Spans[i].Start.Before(view.Spans[j].Start) })
	if len(view.Spans) > maxSpans {
		view.Spans = view.Spans[:maxSpans]
		view.Incomplete = true
	}
	retained := make(map[string]bool, len(view.Spans))
	for _, span := range view.Spans {
		if retained[span.ID] {
			view.Incomplete = true
		}
		retained[span.ID] = true
	}
	for index := range view.Spans {
		span := &view.Spans[index]
		if span.ParentID != "" && !retained[span.ParentID] {
			span.ParentMissing = true
			view.Incomplete = true
		}
	}
	return view, nil
}
