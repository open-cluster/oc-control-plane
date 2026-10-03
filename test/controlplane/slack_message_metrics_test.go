package controlplane

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestSlackMessageMetricsUseTheBoundedWebhookContract(t *testing.T) {
	vendor := newVendorFake(t, "xoxb-installed-token")
	vendor.grant("channels:read,channels:history,users:read")
	plane := startSlackEventPlane(t, vendor)
	plane.connectWorkspace(t)
	if status, body := plane.deliverEvent(t,
		mention("<@U0BOT> inspect the delay", "CMETRICS", "1700000010.123", "", "U9SRE")); status != http.StatusAccepted {
		t.Fatalf("accepting Slack Message = %d: %s", status, body)
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		status, body := plane.get(t, "/metrics")
		if status != http.StatusOK {
			t.Fatalf("GET /metrics = %d: %s", status, body)
		}
		if strings.Contains(body, "oc_webhooks_slack_ack_duration") &&
			strings.Contains(body, "oc_webhooks_delivery_delay") {
			if !strings.Contains(body, "oc_webhooks_requests_total") ||
				!strings.Contains(body, `surface="slack"`) ||
				!strings.Contains(body, `result="accepted"`) ||
				!strings.Contains(body, `otel_scope_name="github.com/open-cluster/oc-control-plane/internal/webhooks"`) {
				t.Fatalf("Slack Message metric names or scope changed: %s", body)
			}
			if strings.Contains(body, `organization=`) || strings.Contains(body, `org_id=`) ||
				strings.Contains(body, "CMETRICS") || strings.Contains(body, "U9SRE") {
				t.Fatalf("Slack Message metrics exposed tenant or caller labels: %s", body)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Slack acknowledgement metric was not published: %s", body)
		}
		time.Sleep(25 * time.Millisecond)
	}
}
