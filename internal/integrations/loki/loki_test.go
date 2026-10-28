package loki_test

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/integrations/loki"
)

func TestVerificationQueriesTheConfiguredTenant(t *testing.T) {
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/proxy/loki/api/v1/query_range" ||
			r.URL.Query().Get("query") != `{job=~".+"}` || r.URL.Query().Get("limit") != "1" ||
			r.Header.Get("Authorization") != "Bearer fixture-token" || r.Header.Get("X-Scope-OrgID") != "production" {
			t.Errorf("verification request = %s %s, headers=%v", r.Method, r.URL, r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[]}}`))
	}))
	defer source.Close()
	verified := loki.Definition().Probe(t.Context(), integrations.ProbeInput{
		Integration: integrations.Integration{Configuration: map[string]any{"url": source.URL + "/proxy", "auth": "bearer", "tenantId": "production"}},
		Credential:  "fixture-token",
	})
	if verified.Status != integrations.StatusVerified || !slices.Contains(verified.Grants, "loki:query") {
		t.Fatalf("verification = %+v", verified)
	}
}

func TestOutcomesAndMalformedResults(t *testing.T) {
	for _, example := range []struct {
		name       string
		status     int
		body, want string
	}{
		{"empty", 200, `{"status":"success","data":{"resultType":"streams","result":[]}}`, ""},
		{"null", 200, `{"status":"success","data":{"resultType":"streams","result":null}}`, "malformed"},
		{"metric", 200, `{"status":"success","data":{"resultType":"matrix","result":[]}}`, "unsupported"},
		{"missing line", 200, `{"status":"success","data":{"resultType":"streams","result":[{"stream":{},"values":[["1700000000000000000",null]]}]}}`, "malformed"},
		{"exclusive end", 200, `{"status":"success","data":{"resultType":"streams","result":[{"stream":{},"values":[["1700000060000000000","outside"]]}]}}`, "out-of-window"},
		{"reflected credential", 200, `{"status":"success","data":{"resultType":"streams","result":[{"stream":{},"values":[["1700000000000000000","fixture-token"]]}]}}`, "credential"},
		{"Basic credential reflection", 200, `{"status":"success","data":{"resultType":"streams","result":[{"stream":{},"values":[["1700000000000000000","Basic MTIzNDU2OmZpeHR1cmUtdG9rZW4="]]}]}}`, "credential"},
		{"syntax", 400, "fixture-token", "rejected"},
		{"denied", 403, "fixture-token", "permission denied"},
		{"rate limit", 429, "fixture-token", "rate limit"},
		{"unavailable", 503, "fixture-token", "unavailable"},
	} {
		t.Run(example.name, func(t *testing.T) {
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(example.status)
				_, _ = w.Write([]byte(example.body))
			}))
			defer source.Close()
			input := queryRequest(source.URL, map[string]any{"query": `{app="checkout"}`})
			input.Credential = "fixture-token"
			if example.name == "Basic credential reflection" {
				input.Integration.Configuration["auth"] = "basic"
				input.Integration.Configuration["username"] = "123456"
			}
			result, err := loki.Definition().Tools[0].Run(t.Context(), input)
			if example.want != "" {
				if err == nil || !strings.Contains(err.Error(), example.want) || strings.Contains(err.Error(), input.Credential) {
					t.Fatalf("unsafe or incorrect outcome: %v", err)
				}
			} else if err != nil || result.Truncated || !strings.Contains(result.Summary, "empty: 0") {
				t.Fatalf("empty outcome = %+v, %v", result, err)
			}
		})
	}
}

func TestLimitsAndDirectionApplyAcrossStreams(t *testing.T) {
	for _, direction := range []string{"forward", "backward"} {
		t.Run(direction, func(t *testing.T) {
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("direction") != direction || r.URL.Query().Get("limit") != "1" {
					t.Errorf("bounded request = %s", r.URL)
				}
				_, _ = fmt.Fprintf(w, `{"status":"success","data":{"resultType":"streams","result":[{"stream":{"app":"one"},"values":[["1700000000000000000",%q]]},{"stream":{"app":"two"},"values":[["1700000001000000000",%q]]}]}}`, strings.Repeat("a", 2200), strings.Repeat("b", 2200))
			}))
			defer source.Close()
			input := queryRequest(source.URL, map[string]any{"query": `{app=~".+"}`, "limit": float64(1), "direction": direction})
			input.WindowFrom = input.WindowUntil.Add(-48 * time.Hour)
			result, err := loki.Definition().Tools[0].Run(t.Context(), input)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(result.Content)
			var content struct {
				Entries []struct {
					Line   string
					Labels map[string]string
				}
				Outcome string
			}
			if err := json.Unmarshal(body, &content); err != nil {
				t.Fatal(err)
			}
			wantApp := "one"
			if direction == "backward" {
				wantApp = "two"
			}
			if !result.Truncated || content.Outcome != "incomplete" || len(content.Entries) != 1 ||
				content.Entries[0].Labels["app"] != wantApp || len([]rune(content.Entries[0].Line)) != 2001 ||
				!result.WindowFrom.Equal(input.WindowUntil.Add(-24*time.Hour)) {
				t.Fatalf("limits/order = %s, result=%+v", body, result)
			}
		})
	}
}

func TestInvalidInputsAndCancellationDoNotReachTheSource(t *testing.T) {
	var calls atomic.Int32
	source := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer source.Close()
	for _, args := range []map[string]any{
		{"query": `{app="checkout"}`, "tenantId": "another"},
		{"query": `{app="checkout"}`, "url": source.URL},
		{"query": `{app="checkout"}`, "limit": float64(201)},
		{"query": `{app="checkout"}`, "end": "2023-11-14T22:00:00Z"},
	} {
		if _, err := loki.Definition().Tools[0].Run(t.Context(), queryRequest(source.URL, args)); err == nil {
			t.Errorf("invalid arguments accepted: %v", args)
		}
	}
	for _, changed := range []map[string]any{
		{"url": source.URL + "?secret=fixture-token"}, {"url": "http://user:fixture-token@example.com"},
		{"tenantId": "one|two"}, {"tenantId": "bad\r\nheader"}, {"auth": "implicit"}, {"auth": "basic", "username": ""},
	} {
		input := queryRequest(source.URL, nil)
		for key, value := range changed {
			input.Integration.Configuration[key] = value
		}
		verified := loki.Definition().Probe(t.Context(), integrations.ProbeInput{Integration: input.Integration})
		if verified.Status != integrations.StatusFailed || strings.Contains(verified.Note, "fixture-token") {
			t.Errorf("invalid configuration = %+v", verified)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := loki.Definition().Tools[0].Run(ctx, queryRequest(source.URL, map[string]any{"query": `{app="checkout"}`})); err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("cancellation = %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid requests reached source %d times", calls.Load())
	}
}

func TestTransportRefusesRedirectsUntrustedTLSAndOversizedDecodedBodies(t *testing.T) {
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
		input := queryRequest(example.address, nil)
		verified := loki.Definition().Probe(t.Context(), integrations.ProbeInput{Integration: input.Integration, Credential: "fixture-token"})
		if verified.Status != integrations.StatusFailed || !strings.Contains(verified.Note, example.want) || strings.Contains(verified.Note, "fixture-token") {
			t.Errorf("transport protection = %+v", verified)
		}
	}
	if forwarded.Load() != 0 {
		t.Fatal("unsafe destination received a request")
	}
}

func TestLogQueriesPreserveLabelsNanosecondsAndAppliedWindow(t *testing.T) {
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked := r.URL.Query()
		if asked.Get("query") != `{app="checkout"} |= "error"` || asked.Get("start") != "1700000000000000000" ||
			asked.Get("end") != "1700000060000000000" || asked.Get("limit") != "100" || asked.Get("direction") != "backward" {
			t.Errorf("log request = %s", r.URL)
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[{"stream":{"app":"checkout"},"values":[["1700000000000000001","an error"]]}]}}`))
	}))
	defer source.Close()
	input := queryRequest(source.URL, map[string]any{"query": `{app="checkout"} |= "error"`, "start": "2023-11-14T22:00:00Z", "end": "2023-11-14T23:00:00Z"})
	definition := loki.Definition()
	if len(definition.Tools) != 1 {
		t.Fatalf("want one log query Tool, got %d", len(definition.Tools))
	}
	result, err := definition.Tools[0].Run(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(result.Content)
	var content struct {
		Outcome string `json:"outcome"`
		Entries []struct {
			Timestamp time.Time         `json:"timestamp"`
			Line      string            `json:"line"`
			Labels    map[string]string `json:"labels"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(body, &content); err != nil {
		t.Fatal(err)
	}
	if content.Outcome != "data" || len(content.Entries) != 1 || content.Entries[0].Line != "an error" ||
		content.Entries[0].Timestamp.UnixNano() != 1700000000000000001 || content.Entries[0].Labels["app"] != "checkout" ||
		result.Truncated || !result.WindowFrom.Equal(input.WindowFrom) || !result.WindowUntil.Equal(input.WindowUntil) {
		t.Fatalf("log observations = %s, result=%+v", body, result)
	}
}

func queryRequest(address string, arguments map[string]any) integrations.ToolRequest {
	return integrations.ToolRequest{
		Integration: integrations.Integration{Configuration: map[string]any{"url": address, "auth": "bearer"}},
		Arguments:   arguments,
		WindowFrom:  time.Unix(1700000000, 0).UTC(), WindowUntil: time.Unix(1700000060, 0).UTC(),
	}
}

func TestBasicAuthenticationUsesTheOneCredential(t *testing.T) {
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, token, ok := r.BasicAuth()
		if !ok || username != "123456" || token != "fixture-token" || r.Header.Get("X-Scope-OrgID") != "production" {
			t.Error("Basic authentication or configured tenant was not applied")
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[]}}`))
	}))
	defer source.Close()
	input := queryRequest(source.URL, map[string]any{"query": `{app="checkout"}`})
	input.Credential = "fixture-token"
	input.Integration.Configuration["auth"] = "basic"
	input.Integration.Configuration["username"] = "123456"
	input.Integration.Configuration["tenantId"] = "production"
	verified := loki.Definition().Probe(t.Context(), integrations.ProbeInput{Integration: input.Integration, Credential: input.Credential})
	if verified.Status != integrations.StatusVerified {
		t.Fatalf("Basic verification = %+v", verified)
	}
	if _, err := loki.Definition().Tools[0].Run(t.Context(), input); err != nil {
		t.Fatal(err)
	}
}
