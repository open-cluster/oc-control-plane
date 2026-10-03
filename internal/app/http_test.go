package app

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/correlation"
)

func TestWebhookSurfaceCorrelatesUnmatchedRequests(t *testing.T) {
	handler := webhookSurface(assembled{logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
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
}

func TestWebhookSurfaceCorrelatesRateLimitedRequests(t *testing.T) {
	handler := webhookSurface(assembled{logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	for range 2_000 {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/webhooks/unknown", nil)
		request.Header.Set(correlation.Header, "attacker-supplied")
		handler.ServeHTTP(response, request)
		if response.Code == http.StatusTooManyRequests {
			requestID := response.Header().Get(correlation.Header)
			if len(requestID) != 32 || requestID == "attacker-supplied" {
				t.Fatalf("rate-limited webhook request ID = %q", requestID)
			}
			return
		}
		if response.Code != http.StatusNotFound {
			t.Fatalf("webhook request = %d", response.Code)
		}
	}
	t.Fatal("webhook surface did not enforce its bounded burst")
}
