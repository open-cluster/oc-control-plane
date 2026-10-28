package tempo_test

import (
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/integrations/tempo"
)

func TestConnectionVerifiesAndQueriesTheConfiguredSource(t *testing.T) {
	for _, sourceConfig := range []struct{ name, auth, token, tenant string }{
		{"bearer", "bearer", "fixture-token", "production"},
		{"basic", "basic", "fixture-token", "production"},
		{"unauthenticated", "bearer", "", ""},
	} {
		t.Run(sourceConfig.name, func(t *testing.T) {
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Query().Get("start") == "" || r.URL.Query().Get("end") == "" ||
					r.Header.Get("Accept") != "application/json" || r.Header.Get("X-Scope-OrgID") != sourceConfig.tenant {
					t.Errorf("connection request = %s %s", r.Method, r.URL)
				}
				if sourceConfig.auth == "basic" {
					user, token, ok := r.BasicAuth()
					if !ok || user != "123456" || token != sourceConfig.token {
						t.Error("Basic credential missing")
					}
				} else {
					wanted := ""
					if sourceConfig.token != "" {
						wanted = "Bearer " + sourceConfig.token
					}
					if r.Header.Get("Authorization") != wanted {
						t.Error("Bearer credential was not applied correctly")
					}
				}
				switch r.URL.Path {
				case "/prefix/api/search":
					if r.URL.Query().Get("q") != "{}" || (r.URL.Query().Get("limit") != "1" && r.URL.Query().Get("limit") != "20") {
						t.Errorf("search parameters = %s", r.URL)
					}
					_, _ = w.Write([]byte(`{"traces":[{"traceID":"1","startTimeUnixNano":"1700000000000000000"}],"metrics":{"completedJobs":1,"totalJobs":1}}`))
				case "/prefix/api/v2/traces/00000000000000000000000000000001":
					_, _ = w.Write([]byte(traceFixture))
				default:
					t.Errorf("unexpected connection path: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer source.Close()
			configuration := map[string]any{"url": source.URL + "/prefix/", "auth": sourceConfig.auth, "tenantId": sourceConfig.tenant}
			if sourceConfig.auth == "basic" {
				configuration["username"] = "123456"
			}
			definition := tempo.Definition()
			verified := definition.Probe(t.Context(), integrations.ProbeInput{Integration: integrations.Integration{Configuration: configuration}, Credential: sourceConfig.token})
			if verified.Status != integrations.StatusVerified || !slices.Contains(verified.Grants, "tempo:query") {
				t.Fatalf("verification = %+v", verified)
			}
			for _, operation := range definition.Tools {
				arguments := map[string]any{"query": "{}"}
				if operation.Name == "tempo_get_trace" {
					arguments = map[string]any{"traceId": "1"}
				}
				input := request(source.URL, arguments)
				input.Integration.Configuration = configuration
				input.Credential = sourceConfig.token
				result, err := operation.Run(t.Context(), input)
				if err != nil {
					t.Fatalf("%s after verification: %v", operation.Name, err)
				}
				encoded, _ := json.Marshal(result.Content)
				if len(result.Sources) == 0 || !strings.Contains(string(encoded), "00000000000000000000000000000001") {
					t.Fatalf("%s did not return source evidence: %s", operation.Name, encoded)
				}
			}
		})
	}
}

func TestOutcomesRejectMalformedAndSensitiveData(t *testing.T) {
	for _, example := range []struct {
		name, tool          string
		status              int
		body, want, outcome string
	}{
		{"empty", "tempo_search_traces", 200, `{"metrics":{"completedJobs":1,"totalJobs":1}}`, "", "empty"},
		{"missing search", "tempo_search_traces", 200, `{}`, "malformed", ""},
		{"malformed trace metadata", "tempo_search_traces", 200, `{"traces":[{}]}`, "identifier", ""},
		{"partial search", "tempo_search_traces", 200, `{"traces":[],"metrics":{"completedJobs":1,"totalJobs":2}}`, "", "incomplete"},
		{"unknown error counts", "tempo_search_traces", 200, `{"traces":[{"traceID":"1","startTimeUnixNano":"1700000000000000000","serviceStats":{"checkout":null}}]}`, "", "data"},
		{"not found", "tempo_get_trace", 404, "fixture-token", "", "not_found"},
		{"missing trace", "tempo_get_trace", 200, `{"trace":null}`, "malformed", ""},
		{"trace mismatch", "tempo_get_trace", 200, strings.ReplaceAll(traceFixture, "AAAAAAAAAAAAAAAAAAAAAQ==", "AAAAAAAAAAAAAAAAAAAAAg=="), "identifier", ""},
		{"invalid duration", "tempo_get_trace", 200, strings.ReplaceAll(traceFixture, "1700000000300000000", "1600000000000000000"), "timestamp or duration", ""},
		{"unknown status", "tempo_get_trace", 200, strings.ReplaceAll(traceFixture, "STATUS_CODE_ERROR", "OTHER_STATUS"), "unsupported span status", ""},
		{"missing parent", "tempo_get_trace", 200, strings.ReplaceAll(strings.ReplaceAll(traceFixture, `"parentSpanId":"AAAAAAAAAAE="`, `"parentSpanId":"AAAAAAAAAAM="`), `"PARTIAL"`, `"COMPLETE"`), "", "incomplete"},
		{"credential reflection", "tempo_get_trace", 200, strings.ReplaceAll(traceFixture, "database unavailable", "fixture-token"), "credential", ""},
		{"Basic reflection", "tempo_get_trace", 200, strings.ReplaceAll(traceFixture, "database unavailable", "Basic MTIzNDU2OmZpeHR1cmUtdG9rZW4="), "credential", ""},
		{"denied", "tempo_search_traces", 403, "fixture-token", "permission denied", ""},
		{"syntax", "tempo_search_traces", 400, "fixture-token", "rejected", ""},
		{"rate limit", "tempo_search_traces", 429, "fixture-token", "rate limit", ""},
		{"unavailable", "tempo_get_trace", 503, "fixture-token", "unavailable", ""},
	} {
		t.Run(example.name, func(t *testing.T) {
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(example.status)
				_, _ = w.Write([]byte(example.body))
			}))
			defer source.Close()
			args := map[string]any{"query": "{status=error}"}
			if example.tool == "tempo_get_trace" {
				args = map[string]any{"traceId": "1"}
			}
			input := request(source.URL, args)
			input.Credential = "fixture-token"
			if example.name == "Basic reflection" {
				input.Integration.Configuration["auth"] = "basic"
				input.Integration.Configuration["username"] = "123456"
			}
			result, err := tool(t, example.tool).Run(t.Context(), input)
			if example.want != "" {
				if err == nil || !strings.Contains(err.Error(), example.want) || strings.Contains(err.Error(), input.Credential) {
					t.Fatalf("unsafe or incorrect error: %v", err)
				}
				if example.name == "malformed trace metadata" {
					verified := tempo.Definition().Probe(t.Context(), integrations.ProbeInput{Integration: input.Integration, Credential: input.Credential})
					if verified.Status != integrations.StatusFailed || len(verified.Grants) != 0 {
						t.Fatalf("malformed metadata granted access: %+v", verified)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(result.Content)
			var outcome struct{ Outcome string }
			if err := json.Unmarshal(body, &outcome); err != nil {
				t.Fatal(err)
			}
			if outcome.Outcome != example.outcome {
				t.Fatalf("outcome = %s", body)
			}
			if example.name == "unknown error counts" {
				var content struct {
					Traces []struct {
						ServiceStats map[string]*struct{ ErrorCount uint32 }
					}
				}
				if err := json.Unmarshal(body, &content); err != nil {
					t.Fatal(err)
				}
				if content.Traces[0].ServiceStats["checkout"] != nil {
					t.Fatalf("unknown counts became measurements: %s", body)
				}
			}
		})
	}
}

func TestTraceAndOutputLimits(t *testing.T) {
	for _, example := range []struct {
		name, service string
		count         int
		wantError     bool
	}{{"spans", "checkout", 201, false}, {"output", strings.Repeat("s", 2000), 200, true}} {
		t.Run(example.name, func(t *testing.T) {
			spans := make([]map[string]any, example.count)
			for i := range spans {
				spans[i] = map[string]any{"traceId": "AAAAAAAAAAAAAAAAAAAAAQ==", "spanId": base64.StdEncoding.EncodeToString([]byte{0, 0, 0, 0, 0, 0, 0, byte(i + 1)}), "name": "operation", "startTimeUnixNano": "1700000000000000000", "endTimeUnixNano": "1700000000100000000"}
			}
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				payload := map[string]any{"trace": map[string]any{"resourceSpans": []any{map[string]any{"resource": map[string]any{"attributes": []any{map[string]any{"key": "service.name", "value": map[string]any{"stringValue": example.service}}}}, "scopeSpans": []any{map[string]any{"spans": spans}}}}}, "status": "COMPLETE"}
				_ = json.NewEncoder(w).Encode(payload)
			}))
			defer source.Close()
			result, err := tool(t, "tempo_get_trace").Run(t.Context(), request(source.URL, map[string]any{"traceId": "1"}))
			if example.wantError {
				if err == nil || !strings.Contains(err.Error(), "output bounds") {
					t.Fatalf("oversized output = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(result.Content)
			var content struct{ Spans []json.RawMessage }
			if err := json.Unmarshal(body, &content); err != nil {
				t.Fatal(err)
			}
			if !result.Truncated || len(content.Spans) != 200 {
				t.Fatalf("unbounded trace = %s", body)
			}
		})
	}
}

func TestInvalidArgumentsConfigurationAndCancellationDoNotReachSource(t *testing.T) {
	var calls atomic.Int32
	source := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer source.Close()
	for _, example := range []struct {
		tool string
		args map[string]any
	}{
		{"tempo_get_trace", map[string]any{"traceId": "../search"}}, {"tempo_get_trace", map[string]any{"traceId": "00000000"}},
		{"tempo_get_trace", map[string]any{"traceId": strings.Repeat("1", 33)}},
		{"tempo_search_traces", map[string]any{"query": "{}", "tenantId": "another"}},
		{"tempo_search_traces", map[string]any{"query": "{}", "url": source.URL}},
		{"tempo_search_traces", map[string]any{"query": "{}", "limit": float64(51)}},
		{"tempo_search_traces", map[string]any{"query": "{}", "end": "2023-11-14T22:00:00Z"}},
	} {
		if _, err := tool(t, example.tool).Run(t.Context(), request(source.URL, example.args)); err == nil {
			t.Errorf("invalid input accepted: %v", example.args)
		}
	}
	for _, changed := range []map[string]any{{"url": source.URL + "?secret=fixture-token"}, {"url": "http://user:fixture-token@example.com"}, {"tenantId": "one|two"}, {"tenantId": "bad\r\nheader"}, {"auth": "implicit"}, {"auth": "basic", "username": "123456"}} {
		input := request(source.URL, nil)
		for k, v := range changed {
			input.Integration.Configuration[k] = v
		}
		verified := tempo.Definition().Probe(t.Context(), integrations.ProbeInput{Integration: input.Integration})
		if verified.Status != integrations.StatusFailed || strings.Contains(verified.Note, "fixture-token") {
			t.Fatalf("invalid configuration accepted or exposed: %+v", verified)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := tool(t, "tempo_search_traces").Run(ctx, request(source.URL, map[string]any{"query": "{}"})); err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("cancellation = %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid calls reached source %d times", calls.Load())
	}
}

func TestHTTPGuardsAndSearchLimit(t *testing.T) {
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
	for _, example := range []struct{ url, want string }{{redirect.URL, "redirects"}, {tls.URL, "unavailable"}, {large.URL, "size limit"}} {
		input := request(example.url, nil)
		verified := tempo.Definition().Probe(t.Context(), integrations.ProbeInput{Integration: input.Integration, Credential: "fixture-token"})
		if verified.Status != integrations.StatusFailed || !strings.Contains(verified.Note, example.want) {
			t.Errorf("HTTP guard = %+v", verified)
		}
	}
	if forwarded.Load() != 0 {
		t.Fatal("unsafe destination received a request")
	}
	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("limit") != "1" {
			t.Errorf("limit = %s", r.URL)
		}
		_, _ = w.Write([]byte(`{"traces":[{"traceID":"1","startTimeUnixNano":"1700000000000000000"},{"traceID":"2","startTimeUnixNano":"1700000000000000000"}]}`))
	}))
	defer limited.Close()
	input := request(limited.URL, map[string]any{"query": "{}", "limit": float64(1)})
	input.WindowFrom = input.WindowUntil.Add(-48 * time.Hour)
	result, err := tool(t, "tempo_search_traces").Run(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(result.Content)
	var content struct{ Traces []json.RawMessage }
	if err := json.Unmarshal(body, &content); err != nil {
		t.Fatal(err)
	}
	if !result.Truncated || len(content.Traces) != 1 || !result.WindowFrom.Equal(input.WindowUntil.Add(-24*time.Hour)) {
		t.Fatalf("search limit = %s, %+v", body, result)
	}
}

func TestTraceV2PreservesSpanRelationshipsAndPartialStatus(t *testing.T) {
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/traces/00000000000000000000000000000001" || r.URL.Query().Get("start") != "1700000000" || r.Header.Get("Accept") != "application/json" {
			t.Errorf("trace request = %s, accept=%q", r.URL, r.Header.Get("Accept"))
		}
		_, _ = w.Write([]byte(traceFixture))
	}))
	defer source.Close()
	result, err := tool(t, "tempo_get_trace").Run(t.Context(), request(source.URL, map[string]any{"traceId": "1"}))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result.Content)
	var content struct {
		TraceID, BackendStatus, Outcome string
		Spans                           []struct {
			ID, ParentID, Service, Operation, Status, StatusMessage string
			DurationNanos                                           uint64
			Start                                                   time.Time
			ParentMissing                                           bool
		}
	}
	if err := json.Unmarshal(encoded, &content); err != nil {
		t.Fatal(err)
	}
	if content.TraceID != "00000000000000000000000000000001" || content.BackendStatus != "PARTIAL" || content.Outcome != "incomplete" || len(content.Spans) != 2 ||
		content.Spans[1].ParentID != "0000000000000001" || content.Spans[1].Service != "checkout" || content.Spans[1].Status != "error" ||
		content.Spans[1].StatusMessage != "database unavailable" || content.Spans[1].DurationNanos != 200000000 || content.Spans[1].Start.UnixNano() != 1700000000100000000 ||
		content.Spans[1].ParentMissing || !result.Truncated || len(result.Sources) != 1 {
		t.Fatalf("trace observations = %s, result=%+v", encoded, result)
	}
}

const traceFixture = `{"trace":{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"checkout"}}]},"scopeSpans":[{"spans":[{"traceId":"AAAAAAAAAAAAAAAAAAAAAQ==","spanId":"AAAAAAAAAAE=","name":"GET /pay","startTimeUnixNano":"1700000000000000000","endTimeUnixNano":"1700000000500000000"},{"traceId":"AAAAAAAAAAAAAAAAAAAAAQ==","spanId":"AAAAAAAAAAI=","parentSpanId":"AAAAAAAAAAE=","name":"database","startTimeUnixNano":"1700000000100000000","endTimeUnixNano":"1700000000300000000","status":{"code":"STATUS_CODE_ERROR","message":"database unavailable"}}]}]}]},"status":"PARTIAL","message":"backend returned a partial trace"}`

func TestSearchUsesSecondsAndReturnsTraceEvidence(t *testing.T) {
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked := r.URL.Query()
		if r.URL.Path != "/api/search" || asked.Get("q") != "{ status=error }" || asked.Get("start") != "1700000000" || asked.Get("end") != "1700000060" || asked.Get("limit") != "20" {
			t.Errorf("search = %s", r.URL)
		}
		_, _ = w.Write([]byte(`{"traces":[{"traceID":"1","rootServiceName":"checkout","rootTraceName":"GET /pay","startTimeUnixNano":"1700000000000000000","durationMs":100,"serviceStats":{"checkout":{"spanCount":3,"errorCount":1}},"spanSets":[{"spans":[{"attributes":[{"key":"status","value":{"stringValue":"error"}}]}]}]}],"metrics":{"completedJobs":1,"totalJobs":1}}`))
	}))
	defer source.Close()
	result, err := tool(t, "tempo_search_traces").Run(t.Context(), request(source.URL, map[string]any{"query": "{ status=error }"}))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result.Content)
	var content struct {
		Traces []struct {
			TraceID, RootService, Operation string
			Start                           time.Time
			DurationMs                      *uint32
			ServiceStats                    map[string]struct{ ErrorCount uint32 }
			ErrorHints                      []string
		}
		Outcome string
	}
	if err := json.Unmarshal(encoded, &content); err != nil {
		t.Fatal(err)
	}
	if content.Outcome != "data" || len(content.Traces) != 1 || content.Traces[0].TraceID != "00000000000000000000000000000001" ||
		content.Traces[0].RootService != "checkout" || content.Traces[0].Operation != "GET /pay" ||
		content.Traces[0].DurationMs == nil || *content.Traces[0].DurationMs != 100 || content.Traces[0].Start.Unix() != 1700000000 ||
		content.Traces[0].ServiceStats["checkout"].ErrorCount != 1 || len(content.Traces[0].ErrorHints) != 1 || result.Truncated || len(result.Sources) == 0 {
		t.Fatalf("search observations = %s, result=%+v", encoded, result)
	}
}

func tool(t *testing.T, name string) integrations.Tool {
	t.Helper()
	for _, candidate := range tempo.Definition().Tools {
		if candidate.Name == name {
			return candidate
		}
	}
	t.Fatalf("missing Tool %s", name)
	return integrations.Tool{}
}

func request(address string, arguments map[string]any) integrations.ToolRequest {
	return integrations.ToolRequest{
		Integration: integrations.Integration{Configuration: map[string]any{"url": address, "auth": "bearer"}}, Arguments: arguments,
		WindowFrom: time.Unix(1700000000, 0).UTC(), WindowUntil: time.Unix(1700000060, 0).UTC(),
	}
}
