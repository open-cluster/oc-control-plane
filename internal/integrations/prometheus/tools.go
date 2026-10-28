package prometheus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	prom "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

const (
	maxSeries      = 50
	maxSamples     = 240
	maxWindow      = 24 * time.Hour
	maxResultBytes = 256 << 10
)

type point struct {
	Timestamp time.Time `json:"timestamp"`
	Value     string    `json:"value"`
}

type series struct {
	Labels  model.Metric `json:"labels"`
	Samples []point      `json:"samples"`
}

type queryContent struct {
	Query       string    `json:"query"`
	Mode        string    `json:"mode"`
	ResultType  string    `json:"resultType"`
	From        time.Time `json:"from"`
	Until       time.Time `json:"until"`
	StepSeconds int       `json:"stepSeconds,omitempty"`
	Series      []series  `json:"series"`
	Warnings    []string  `json:"warnings,omitempty"`
	Infos       []string  `json:"infos,omitempty"`
	Incomplete  bool      `json:"incomplete"`
}

func queryTool() integrations.Tool {
	declared := []integrations.ToolArgument{
		{Name: "query", Type: integrations.FieldString, Required: true,
			Description: "Read-only PromQL expression, at most 8192 bytes. Discover relevant metric names first when needed."},
		{Name: "mode", Type: integrations.FieldString, Required: true,
			Description: "instant evaluates at end; range evaluates start through end. Must be instant or range."},
		{Name: "start", Type: integrations.FieldString,
			Description: "Optional RFC 3339 start, clamped to the Investigation window. Range reads cover at most the latest 24 hours."},
		{Name: "end", Type: integrations.FieldString,
			Description: "Optional RFC 3339 end or instant evaluation time, clamped to the Investigation window."},
		{Name: "step", Type: integrations.FieldInteger,
			Description: "Optional range step in whole seconds, 1–86400. Raised when needed to evaluate at most 240 timestamps per series."},
	}
	return integrations.Tool{
		Name:           "prometheus_query",
		RequiredGrants: []string{queryGrant},
		Arguments:      declared,
		Description:    "Evaluate read-only PromQL as an instant or range query against the selected Prometheus Integration. Use metrics to compare errors, latency and resource usage with other evidence. Returns labeled string-valued samples, effective evaluation times, annotations and incomplete-result indicators; NaN/Inf are not zero. At most 50 series and 240 samples per series. Evaluation timestamps are bounded, but PromQL offsets, range selectors, subqueries and lookback can read earlier data; this is not strict historical isolation. No matching data does not prove health or causation.",
		Run: func(ctx context.Context, request integrations.ToolRequest) (integrations.ToolResult, error) {
			arguments, err := integrations.ReadArguments(declared, request.Arguments)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			query, err := arguments.Required("query")
			if err != nil || len(query) > 8192 {
				return integrations.ToolResult{}, errors.New("query must contain 1–8192 bytes of PromQL")
			}
			mode, err := arguments.Required("mode")
			if err != nil || (mode != "instant" && mode != "range") {
				return integrations.ToolResult{}, errors.New("mode must be instant or range")
			}
			from, until, limited, err := queryWindow(request, arguments)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			minimumStep := max(1, int(math.Ceil(until.Sub(from).Seconds()/float64(maxSamples-1))))
			step, err := arguments.Count("step", minimumStep, int(maxWindow/time.Second))
			if err != nil {
				return integrations.ToolResult{}, err
			}
			address, _ := request.Integration.Configuration["url"].(string)

			client, err := newClient(address, request.Credential)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			var value model.Value
			var warnings prom.Warnings
			var infos prom.Infos
			if mode == "instant" {
				from, step, limited = until, 0, false
				value, warnings, infos, err = client.Query(ctx, query, until, prom.WithLimit(maxSeries+1), prom.WithTimeout(queryTimeout))
			} else {
				if step < minimumStep {
					step, limited = minimumStep, true
				}
				value, warnings, infos, err = client.QueryRange(ctx, query,
					prom.Range{
						Start: from,
						End:   until,
						Step:  time.Duration(step) * time.Second,
					}, prom.WithLimit(maxSeries+1), prom.WithTimeout(queryTimeout))
			}
			if err != nil {
				return integrations.ToolResult{}, queryError(err)
			}
			normalized, trimmed, err := normalize(value)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			limited = limited || trimmed || len(warnings) > 0 || len(infos) > 0
			content := queryContent{
				Query:       query,
				Mode:        mode,
				ResultType:  value.Type().String(),
				From:        from,
				Until:       until,
				StepSeconds: step,
				Series:      normalized,
				Incomplete:  limited,
				Warnings:    annotations(warnings, request.Credential),
				Infos:       annotations(infos, request.Credential),
			}
			if err := checkOutput(content, request.Credential); err != nil {
				return integrations.ToolResult{}, err
			}
			count := 0
			for _, one := range normalized {
				count += len(one.Samples)
			}
			summary := fmt.Sprintf("Prometheus %s %q: %d series, %d samples; %s to %s; incomplete=%t",
				mode, boundedText(query, 120), len(normalized), count, from.Format(time.RFC3339), until.Format(time.RFC3339), limited)
			if len(normalized) > 0 && len(normalized[0].Samples) > 0 {
				last := normalized[0].Samples[len(normalized[0].Samples)-1]
				summary += fmt.Sprintf("; retained %s=%s at %s", boundedText(normalized[0].Labels.String(), 80), last.Value, last.Timestamp.Format(time.RFC3339))
			}
			return integrations.ToolResult{
				Content:     content,
				Truncated:   limited,
				Summary:     summary,
				WindowFrom:  from,
				WindowUntil: until,
				Sources:     []string{"prometheus:" + request.Integration.ID.String() + ":" + mode},
			}, nil
		},
	}
}

func normalize(value model.Value) ([]series, bool, error) {
	result := make([]series, 0)
	limited := false
	switch typed := value.(type) {
	case model.Vector:
		if typed == nil {
			return nil, false, errors.New("prometheus returned a missing vector result")
		}
		if len(typed) > maxSeries {
			typed, limited = typed[:maxSeries], true
		}
		for _, sample := range typed {
			// The SDK decodes an absent vector value as (0, 0); refuse that ambiguity.
			if sample == nil || (sample.Histogram == nil && sample.Timestamp == 0 && sample.Value == 0) {
				return nil, false, errors.New("prometheus returned a missing or ambiguous sample")
			}
			if sample.Histogram != nil {
				return nil, false, errors.New(
					"prometheus native histogram samples are not supported; query a float-valued expression")
			}
			result = append(result, series{
				Labels: sample.Metric,
				Samples: []point{{
					Timestamp: sample.Timestamp.Time().UTC(),
					Value:     sample.Value.String(),
				}}})
		}
	case model.Matrix:
		if typed == nil {
			return nil, false, errors.New("prometheus returned a missing matrix result")
		}
		if len(typed) > maxSeries {
			typed, limited = typed[:maxSeries], true
		}
		for _, stream := range typed {
			if stream == nil {
				return nil, false, errors.New("prometheus returned a missing sample stream")
			}
			if len(stream.Histograms) > 0 {
				return nil, false, errors.New(
					"prometheus native histogram samples are not supported; query a float-valued expression")
			}
			count := min(len(stream.Values), maxSamples)
			points := make([]point, count)
			for index := range count {
				at := index
				if len(stream.Values) > maxSamples {
					at = index * (len(stream.Values) - 1) / (maxSamples - 1)
				}
				pair := stream.Values[at]
				points[index] = point{
					Timestamp: pair.Timestamp.Time().UTC(),
					Value:     pair.Value.String(),
				}
			}
			limited = limited || len(stream.Values) > maxSamples
			result = append(result, series{
				Labels:  stream.Metric,
				Samples: points,
			})
		}
	case *model.Scalar:
		result = append(result, series{
			Labels: model.Metric{},
			Samples: []point{{
				Timestamp: typed.Timestamp.Time().UTC(),
				Value:     typed.Value.String(),
			}}})
	default:
		return nil, false, errors.New("prometheus returned an unsupported result shape")
	}
	return result, limited, nil
}

func discoverMetricsTool() integrations.Tool {
	declared := []integrations.ToolArgument{
		{Name: "hint", Type: integrations.FieldString, Required: true,
			Description: "Required literal metric-name hint, such as http, cpu or memory. At most 128 bytes. Discovery only returns names containing this text."},
		{Name: "selector", Type: integrations.FieldString,
			Description: `Optional label matchers such as {job="checkout"}, at most 2048 bytes. Use to scope discovery to the relevant service.`},
	}
	return integrations.Tool{
		Name:           "prometheus_discover_metrics",
		RequiredGrants: []string{queryGrant},
		Arguments:      declared,
		Description:    "Discover up to 50 metric names matching a literal hint and optional service label matchers in the Investigation window, capped at 24 hours. Use before constructing PromQL when metric names are unknown. This returns names, not measurements. Warnings and limits indicate incomplete discovery; absence does not prove a metric never existed.",
		Run: func(ctx context.Context, request integrations.ToolRequest) (integrations.ToolResult, error) {
			arguments, err := integrations.ReadArguments(declared, request.Arguments)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			hint, err := arguments.Required("hint")
			if err != nil || len(hint) > 128 {
				return integrations.ToolResult{}, errors.New("hint must contain 1–128 bytes of literal metric-name text")
			}
			selector, err := arguments.Text("selector")
			if err != nil || len(selector) > 2048 || (selector != "" &&
				(!strings.HasPrefix(selector, "{") || !strings.HasSuffix(selector, "}"))) {
				return integrations.ToolResult{}, errors.New("selector must be label matchers enclosed in braces, at most 2048 bytes")
			}
			match := "{__name__=~" + strconv.Quote(".*"+regexp.QuoteMeta(hint)+".*")
			if selector != "" && strings.TrimSpace(selector[1:len(selector)-1]) != "" {
				match += "," + selector[1:len(selector)-1]
			}
			match += "}"
			from, until, limited, err := queryWindow(request, arguments)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			address, _ := request.Integration.Configuration["url"].(string)
			client, err := newClient(address, request.Credential)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			names, warnings, infos, err := client.LabelValues(ctx, "__name__", []string{match}, from, until, prom.WithLimit(maxSeries+1))
			if err != nil {
				return integrations.ToolResult{}, queryError(err)
			}
			limited = limited || len(names) > maxSeries || len(warnings) > 0 || len(infos) > 0
			if len(names) > maxSeries {
				names = names[:maxSeries]
			}
			content := struct {
				Selector   string    `json:"selector"`
				Names      []string  `json:"names"`
				From       time.Time `json:"from"`
				Until      time.Time `json:"until"`
				Warnings   []string  `json:"warnings,omitempty"`
				Infos      []string  `json:"infos,omitempty"`
				Incomplete bool      `json:"incomplete"`
			}{
				Selector:   match,
				Names:      make([]string, 0, len(names)),
				From:       from,
				Until:      until,
				Warnings:   annotations(warnings, request.Credential),
				Infos:      annotations(infos, request.Credential),
				Incomplete: limited,
			}
			for _, name := range names {
				content.Names = append(content.Names, string(name))
			}
			if err := checkOutput(content, request.Credential); err != nil {
				return integrations.ToolResult{}, err
			}
			return integrations.ToolResult{
				Content: content, Truncated: limited, WindowFrom: from, WindowUntil: until,
				Summary: fmt.Sprintf("Prometheus metric discovery %q: %d names, %s to %s; incomplete=%t",
					hint, len(names), from.Format(time.RFC3339), until.Format(time.RFC3339), limited),
				Sources: []string{"prometheus:" + request.Integration.ID.String() + ":metric-names"},
			}, nil
		},
	}
}

func queryWindow(request integrations.ToolRequest, arguments integrations.Arguments) (
	time.Time, time.Time, bool, error) {
	if request.WindowFrom.IsZero() ||
		request.WindowUntil.IsZero() ||
		request.WindowFrom.After(request.WindowUntil) {
		return time.Time{}, time.Time{}, false, errors.New("prometheus reads require a bounded Investigation window")
	}
	from, err := arguments.Moment("start")
	if err != nil {
		return time.Time{}, time.Time{}, false, err
	}
	until, err := arguments.Moment("end")
	if err != nil {
		return time.Time{}, time.Time{}, false, err
	}
	if from.IsZero() {
		from = request.WindowFrom
	}
	if until.IsZero() {
		until = request.WindowUntil
	}
	if from.After(until) || until.Before(request.WindowFrom) || from.After(request.WindowUntil) {
		return time.Time{}, time.Time{}, false, errors.New("requested time interval does not intersect the Investigation window")
	}
	from, until = request.ClampWindow(from, until)
	limited := until.Sub(from) > maxWindow
	if limited {
		from = until.Add(-maxWindow)
	}
	return from.UTC(), until.UTC(), limited, nil
}

func annotations(values []string, credential string) []string {
	if len(values) > 8 {
		values = values[:8]
	}
	bounded := make([]string, 0, len(values))
	for _, value := range values {
		if credential != "" {
			value = strings.ReplaceAll(value, credential, "[redacted]")
		}
		bounded = append(bounded, boundedText(value, 256))
	}
	return bounded
}

func boundedText(value string, limit int) string {
	runes := []rune(value)
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return value
}

func checkOutput(content any, credential string) error {
	encoded, err := json.Marshal(content)
	if err != nil || len(encoded) > maxResultBytes {
		return errors.New("prometheus result exceeds safe output bounds; narrow the query")
	}
	if credential != "" {
		encodedCredential, _ := json.Marshal(credential)
		if bytes.Contains(encoded, encodedCredential[1:len(encodedCredential)-1]) {
			return errors.New("prometheus result could expose its credential; response withheld")
		}
	}
	return nil
}
