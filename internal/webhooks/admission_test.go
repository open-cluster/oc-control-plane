package webhooks

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebhookAdmissionBoundsUnauthenticatedIdentifiersAcrossEndpoints(t *testing.T) {
	router := (Handlers{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Slack:  &SlackAgent{SigningSecret: "test-signing-secret"},
	}).Router()
	limited := map[string]bool{}
	for n := range 1200 {
		path := fmt.Sprintf("/webhooks/v1/integrations/unknown-%d/alert-events", n)
		endpoint := "alert-events"
		if n%2 == 0 {
			path = SlackEventsPath
			endpoint = "slack"
		}
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		request.Header.Set("X-Forwarded-For", fmt.Sprintf("192.0.2.%d", n))
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code == http.StatusTooManyRequests {
			if response.Header().Get("Retry-After") != "1" {
				t.Fatal("admission refusal omitted Retry-After")
			}
			limited[endpoint] = true
		} else if response.Code != http.StatusUnauthorized {
			t.Fatalf("request %d = %d, want 401 or 429", n, response.Code)
		}
	}
	if !limited["slack"] || !limited["alert-events"] {
		t.Fatalf("unauthenticated admission was not bounded on both endpoints: %v", limited)
	}
}
