package app

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/correlation"
)

func TestWebhookSurfaceCorrelatesUnmatchedRequests(t *testing.T) {
	var logs bytes.Buffer
	handler := webhookSurface(assembled{logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	request := httptest.NewRequest(http.MethodPost, "/webhooks/unknown", nil)
	request.Header.Set(correlation.Header, "attacker-supplied")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNotFound {
		t.Fatalf("unmatched webhook status = %d", response.Code)
	}
	requestID := response.Header().Get(correlation.Header)
	if len(requestID) != 32 || requestID == "attacker-supplied" {
		t.Fatalf("unmatched webhook request ID = %q", requestID)
	}
	assertRequestLog(t, logs.String(), requestID, http.StatusNotFound)
}

func TestWebhookSurfaceRejectsBeforeReadingRateLimitedAlerts(t *testing.T) {
	handler := webhookSurface(assembled{logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	for range 2_000 {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/webhooks/unknown", nil)
		request.Header.Set(correlation.Header, "attacker-supplied")
		handler.ServeHTTP(response, request)
		if response.Code == http.StatusTooManyRequests {
			body := &trackingBody{}
			alert := httptest.NewRequest(http.MethodPost,
				"/webhooks/v1/integrations/00000000-0000-0000-0000-000000000001/alert-events", body)
			limited := httptest.NewRecorder()
			handler.ServeHTTP(limited, alert)
			if limited.Code != http.StatusServiceUnavailable {
				t.Fatalf("rate-limited alert status = %d", limited.Code)
			}
			if body.read {
				t.Fatal("rate-limited alert body was read before admission")
			}
			if requestID := limited.Header().Get(correlation.Header); len(requestID) != 32 {
				t.Fatalf("rate-limited alert request ID = %q", requestID)
			}
			return
		}
		if response.Code != http.StatusNotFound {
			t.Fatalf("webhook request = %d", response.Code)
		}
	}
	t.Fatal("webhook surface did not enforce its bounded burst")
}

type trackingBody struct{ read bool }

func (b *trackingBody) Read([]byte) (int, error) {
	b.read = true
	return 0, io.EOF
}

func (b *trackingBody) Close() error { return nil }

func assertRequestLog(t *testing.T, logs, requestID string, status int) {
	t.Helper()
	for line := range strings.Lines(logs) {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decode request log: %v", err)
		}
		if entry["msg"] == "request served" && entry["request_id"] == requestID {
			if entry["status"] != float64(status) {
				t.Fatalf("logged status = %v", entry["status"])
			}
			return
		}
	}
	t.Fatalf("no request log carried request ID %q: %s", requestID, logs)
}
