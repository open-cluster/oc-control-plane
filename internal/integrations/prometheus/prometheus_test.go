package prometheus_test

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/integrations/prometheus"
)

func TestVerificationQueriesTheConfiguredSource(t *testing.T) {
	for _, token := range []string{"", "fixture-bearer"} {
		t.Run("token="+token, func(t *testing.T) {
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil {
					t.Error(err)
				}
				if r.URL.Path != "/prometheus/api/v1/query" || r.Form.Get("query") != "vector(1)" {
					t.Errorf("verification request = %s %s, %v", r.Method, r.URL.Path, r.Form)
				}
				wantAuth := ""
				if token != "" {
					wantAuth = "Bearer " + token
				}
				if r.Header.Get("Authorization") != wantAuth {
					t.Error("verification did not use the configured credential")
				}
				if token != "" && r.Method == http.MethodPost {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1700000000,"1"]}]}}`))
			}))
			defer source.Close()
			definition := prometheus.Definition()
			verified := definition.Probe(t.Context(), integrations.ProbeInput{
				Integration: integrations.Integration{Configuration: map[string]any{"url": source.URL + "/prometheus"}},
				Credential:  token,
			})
			if verified.Status != integrations.StatusVerified || !slices.Contains(verified.Grants, "prometheus:query") {
				t.Fatalf("verification = %+v", verified)
			}
		})
	}
}

func TestQueriesNormalizeTheSDKResultsAndEvaluationWindow(t *testing.T) {
	for _, example := range []struct {
		name, mode, payload, value string
	}{
		{"vector", "instant", `{"resultType":"vector","result":[{"metric":{"job":"checkout"},"value":[1700000000,"NaN"]}]}`, "NaN"},
		{"scalar", "instant", `{"resultType":"scalar","result":[1700000000,"+Inf"]}`, "+Inf"},
		{"matrix", "range", `{"resultType":"matrix","result":[{"metric":{"job":"checkout"},"values":[[1700000000,"1.5"],[1700000010,"2.5"]]}]}`, "1.5"},
	} {
		t.Run(example.name, func(t *testing.T) {
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil {
					t.Error(err)
				}
				if r.Form.Get("query") != `rate(http_requests_total{job="checkout"}[5m])` || r.Form.Get("limit") != "51" {
					t.Errorf("query parameters = %v", r.Form)
				}
				if example.mode == "range" {
					bounds := request("", nil)
					if r.URL.Path != "/api/v1/query_range" || r.Form.Get("step") != "31" ||
						r.Form.Get("start") != strconv.FormatInt(bounds.WindowFrom.Unix(), 10) ||
						r.Form.Get("end") != strconv.FormatInt(bounds.WindowUntil.Unix(), 10) {
						t.Errorf("range parameters = %s, %v", r.URL.Path, r.Form)
					}
				} else if r.URL.Path != "/api/v1/query" || r.Form.Get("time") == "" {
					t.Errorf("instant parameters = %s, %v", r.URL.Path, r.Form)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"status":"success","data":%s}`, example.payload)
			}))
			defer source.Close()
			input := request(source.URL, map[string]any{
				"query": `rate(http_requests_total{job="checkout"}[5m])`, "mode": example.mode,
				"start": "2026-10-28T09:00:00Z", "end": "2026-10-28T13:00:00Z",
			})
			result, err := tool(t, "prometheus_query").Run(t.Context(), input)
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(result.Content)
			var content struct {
				Query  string `json:"query"`
				Series []struct {
					Labels  map[string]string `json:"labels"`
					Samples []struct {
						Value string `json:"value"`
					} `json:"samples"`
				} `json:"series"`
			}
			if err := json.Unmarshal(encoded, &content); err != nil {
				t.Fatal(err)
			}
			if content.Query == "" || len(content.Series) != 1 || len(content.Series[0].Samples) == 0 ||
				content.Series[0].Samples[0].Value != example.value || !result.WindowUntil.Equal(input.WindowUntil) {
				t.Fatalf("query = %s, result=%+v", encoded, result)
			}
			if example.mode == "range" && !result.WindowFrom.Equal(input.WindowFrom) {
				t.Errorf("evaluation escaped the Investigation: %+v", result)
			}
			if example.name != "scalar" && content.Series[0].Labels["job"] != "checkout" {
				t.Error("series labels were lost")
			}
		})
	}
}

func TestLimitsAreReportedAsIncomplete(t *testing.T) {
	for _, example := range []struct {
		name            string
		series, samples int
		outputTooLarge  bool
	}{
		{"series", 51, 1, false}, {"samples", 1, 250, false}, {"output", 50, 240, true},
	} {
		t.Run(example.name, func(t *testing.T) {
			streams := make([]any, example.series)
			for index := range streams {
				pairs := make([]any, example.samples)
				for sample := range pairs {
					pairs[sample] = []any{1700000000 + sample, strconv.Itoa(sample)}
				}
				streams[index] = map[string]any{"metric": map[string]string{"instance": strconv.Itoa(index)}, "values": pairs}
			}
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": streams}})
			}))
			defer source.Close()
			result, err := tool(t, "prometheus_query").Run(t.Context(), request(source.URL, map[string]any{"query": "up", "mode": "range"}))
			if example.outputTooLarge {
				if err == nil || !strings.Contains(err.Error(), "output bounds") {
					t.Fatalf("oversized normalized result = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(result.Content)
			var content struct {
				Series []struct {
					Samples []struct {
						Value string `json:"value"`
					} `json:"samples"`
				} `json:"series"`
			}
			if err := json.Unmarshal(encoded, &content); err != nil {
				t.Fatal(err)
			}
			if !result.Truncated || len(content.Series) > 50 || len(content.Series[0].Samples) > 240 ||
				content.Series[0].Samples[len(content.Series[0].Samples)-1].Value != strconv.Itoa(example.samples-1) {
				t.Fatalf("unbounded or misleading result: %+v", result)
			}
		})
	}
}

func TestQueryOutcomesAreDistinctAndErrorsDoNotExposeCredentials(t *testing.T) {
	for _, example := range []struct {
		name       string
		status     int
		body, want string
	}{
		{"empty", 200, `{"status":"success","data":{"resultType":"vector","result":[]}}`, ""},
		{"null vector", 200, `{"status":"success","data":{"resultType":"vector","result":null}}`, "missing"},
		{"null matrix", 200, `{"status":"success","data":{"resultType":"matrix","result":null}}`, "missing"},
		{"missing value", 200, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"job":"checkout"}}]}}`, "missing"},
		{"missing sample", 200, `{"status":"success","data":{"resultType":"vector","result":[null]}}`, "missing"},
		{"missing stream", 200, `{"status":"success","data":{"resultType":"matrix","result":[null]}}`, "missing"},
		{"credential reflection", 200, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"authorization":"fixture-bearer"},"value":[1700000000,"1"]}]}}`, "credential"},
		{"syntax", 422, `{"status":"error","errorType":"bad_data","error":"fixture-bearer"}`, "rejected"},
		{"denied", 403, `fixture-bearer`, "permission denied"},
		{"unavailable", 503, `{"status":"error","errorType":"unavailable","error":"fixture-bearer"}`, "unavailable"},
		{"timeout", 503, `{"status":"error","errorType":"timeout","error":"fixture-bearer"}`, "timed out"},
		{"string", 200, `{"status":"success","data":{"resultType":"string","result":[1700000000,"text"]}}`, "unsupported"},
		{"histogram", 200, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"histogram":[1700000000,{"count":"1","sum":"1","buckets":[]}]}]}}`, "histogram"},
	} {
		t.Run(example.name, func(t *testing.T) {
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(example.status)
				_, _ = w.Write([]byte(example.body))
			}))
			defer source.Close()
			input := request(source.URL, map[string]any{"query": "up", "mode": "instant"})
			input.Credential = "fixture-bearer"
			result, err := tool(t, "prometheus_query").Run(t.Context(), input)
			if example.want == "" {
				if err != nil || result.Truncated || !strings.Contains(result.Summary, "0 series, 0 samples") {
					t.Fatalf("empty success = %+v, %v", result, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), example.want) || strings.Contains(err.Error(), input.Credential) {
				t.Fatalf("unsafe or incorrect error: %v", err)
			}
			if example.status == http.StatusForbidden || example.name == "missing sample" {
				verified := prometheus.Definition().Probe(t.Context(), integrations.ProbeInput{Integration: input.Integration, Credential: input.Credential})
				if verified.Status != integrations.StatusFailed || len(verified.Grants) != 0 || strings.Contains(verified.Note, input.Credential) {
					t.Fatalf("failed verification granted access: %+v", verified)
				}
			}
		})
	}
}

func TestInvalidInputsAndCancellationDoNotReachTheSource(t *testing.T) {
	var calls atomic.Int32
	source := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer source.Close()
	for _, example := range []struct {
		tool string
		args map[string]any
	}{
		{"prometheus_discover_metrics", map[string]any{}},
		{"prometheus_query", map[string]any{"query": "up", "mode": "range", "end": "2026-10-28T09:00:00Z"}},
		{"prometheus_query", map[string]any{"query": "up", "mode": "instant", "url": source.URL}},
	} {
		if _, err := tool(t, example.tool).Run(t.Context(), request(source.URL, example.args)); err == nil {
			t.Errorf("invalid request accepted: %v", example.args)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := tool(t, "prometheus_query").Run(ctx, request(source.URL, map[string]any{"query": "up", "mode": "instant"})); err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("cancellation = %v", err)
	}
	unbounded := request(source.URL, map[string]any{"query": "up", "mode": "range"})
	unbounded.WindowFrom = time.Time{}
	if _, err := tool(t, "prometheus_query").Run(t.Context(), unbounded); err == nil {
		t.Error("query accepted an unbounded Investigation")
	}
	for _, address := range []string{source.URL + "?token=fixture-bearer", "ftp://example.com", "http://user:fixture-bearer@example.com"} {
		verified := prometheus.Definition().Probe(t.Context(), integrations.ProbeInput{Integration: request(address, nil).Integration})
		if verified.Status != integrations.StatusFailed || strings.Contains(verified.Note, "fixture-bearer") {
			t.Errorf("invalid URL accepted or exposed: %+v", verified)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid/cancelled inputs made %d requests", calls.Load())
	}
}

func TestTransportRejectsRedirectsUntrustedTLSAndOversizedDecodedBodies(t *testing.T) {
	var forwarded atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { forwarded.Add(1) }))
	defer target.Close()
	redirect := httptest.NewServer(http.RedirectHandler(target.URL, http.StatusFound))
	defer redirect.Close()
	tls := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { forwarded.Add(1) }))
	defer tls.Close()
	large := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		compressed := gzip.NewWriter(w)
		_, _ = compressed.Write([]byte(strings.Repeat("x", 3<<20)))
		_ = compressed.Close()
	}))
	defer large.Close()
	for _, example := range []struct{ address, want string }{
		{redirect.URL, "redirects"}, {tls.URL, "unavailable"}, {large.URL, "size limit"},
	} {
		verified := prometheus.Definition().Probe(t.Context(), integrations.ProbeInput{Integration: request(example.address, nil).Integration, Credential: "fixture-bearer"})
		if verified.Status != integrations.StatusFailed || !strings.Contains(verified.Note, example.want) || strings.Contains(verified.Note, "fixture-bearer") {
			t.Errorf("transport protection = %+v, want %s", verified, example.want)
		}
	}
	if forwarded.Load() != 0 {
		t.Error("redirect or invalid TLS forwarded a credential-bearing request")
	}
}

func tool(t *testing.T, name string) integrations.Tool {
	t.Helper()
	for _, candidate := range prometheus.Definition().Tools {
		if candidate.Name == name {
			return candidate
		}
	}
	t.Fatalf("missing Tool %s", name)
	return integrations.Tool{}
}

func request(address string, arguments map[string]any) integrations.ToolRequest {
	return integrations.ToolRequest{
		Integration: integrations.Integration{Configuration: map[string]any{"url": address}},
		Arguments:   arguments,
		WindowFrom:  time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC),
		WindowUntil: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC),
	}
}

func TestDiscoveryUsesScopedLabelValuesAndPreservesAnnotations(t *testing.T) {
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.URL.Path != "/api/v1/label/__name__/values" ||
			r.Form.Get("match[]") != `{__name__=~".*http.*",job="checkout"}` ||
			r.Form.Get("limit") != "51" || r.Form.Get("start") == "" || r.Form.Get("end") == "" {
			t.Errorf("discovery request = %s, %v", r.URL.Path, r.Form)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":["http_requests_total","http_request_duration_seconds"],"warnings":["partial response fixture-bearer"],"infos":["replica omitted"]}`))
	}))
	defer source.Close()
	input := request(source.URL, map[string]any{"hint": "http", "selector": `{job="checkout"}`})
	input.Credential = "fixture-bearer"
	input.WindowFrom = input.WindowUntil.Add(-48 * time.Hour)
	result, err := tool(t, "prometheus_discover_metrics").Run(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result.Content)
	if err != nil {
		t.Fatal(err)
	}
	var content struct {
		Names    []string `json:"names"`
		Warnings []string `json:"warnings"`
		Infos    []string `json:"infos"`
	}
	if err := json.Unmarshal(encoded, &content); err != nil {
		t.Fatal(err)
	}
	if len(content.Names) != 2 || !result.Truncated || len(content.Warnings) != 1 || len(content.Infos) != 1 ||
		!result.WindowFrom.Equal(input.WindowUntil.Add(-24*time.Hour)) || !result.WindowUntil.Equal(input.WindowUntil) ||
		len(result.Sources) != 1 || strings.Contains(string(encoded), input.Credential) {
		t.Fatalf("discovery = %s, result=%+v", encoded, result)
	}
}
