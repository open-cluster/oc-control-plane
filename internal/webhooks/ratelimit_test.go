package webhooks

import (
	"testing"
	"time"
)

func atTime(start time.Time, elapsed *time.Duration) func() time.Time {
	return func() time.Time { return start.Add(*elapsed) }
}

func TestLimiterBoundsAndRefillsTheWebhookSurface(t *testing.T) {
	var elapsed time.Duration
	limiter := newLimiter(atTime(time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC), &elapsed))
	for range requestBurst {
		if !limiter.allowRequest() {
			t.Fatal("webhook request burst refused early")
		}
	}
	if limiter.allowRequest() {
		t.Fatal("webhook request burst was unbounded")
	}
	elapsed = requestRefillInterval
	if !limiter.allowRequest() || limiter.allowRequest() {
		t.Fatal("one refill interval must restore exactly one request")
	}
}
