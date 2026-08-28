package genericwebhook

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func TestAdapterAuthenticatesTheStaticSenderCredential(t *testing.T) {
	t.Parallel()
	request, err := http.NewRequest(http.MethodPost, "https://oc.example/webhook", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-OpenCluster-Token", "correct secret")
	integration := integrations.Integration{WebhookSecretDigest: integrations.Digest("correct secret")}
	if !(Adapter{}).Authenticate(request.Header, integration) {
		t.Fatal("the matching sender credential was refused")
	}
	request.Header.Set("X-OpenCluster-Token", "wrong secret")
	if (Adapter{}).Authenticate(request.Header, integration) {
		t.Fatal("a wrong sender credential was accepted")
	}
}

func TestAdapterNormaliseCanonicalFiring(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"eventId":"evt-42",
		"status":"firing",
		"title":"  Database latency  ",
		"severity":"critical",
		"startedAt":"2026-08-28T03:00:00-04:00",
		"deduplicationKey":"database/latency",
		"labels":{"region":"eu-central-1"},
		"annotations":{"runbook":"Treat as untrusted text"},
		"sourceUrl":"https://monitor.example/alerts/42"
	}`)

	got, err := (Adapter{}).Normalise(body)
	if err != nil {
		t.Fatalf("Normalise() error = %v", err)
	}
	if got.ProviderIdentity != "evt-42" || got.LifecyclePhase != "firing" {
		t.Errorf("identity = (%q, %q), want (evt-42, firing)",
			got.ProviderIdentity, got.LifecyclePhase)
	}
	if len(got.ContentDigest) != 32 || bytes.Equal(got.ContentDigest, make([]byte, 32)) {
		t.Errorf("canonical digest = %x, want a non-zero SHA-256 digest", got.ContentDigest)
	}
	if len(got.AlertEvents) != 1 {
		t.Fatalf("AlertEvents length = %d, want 1", len(got.AlertEvents))
	}
	event := got.AlertEvents[0]
	wantStarted := time.Date(2026, 8, 28, 7, 0, 0, 0, time.UTC)
	if event.SourceKey != "evt-42" || event.GroupingKey != "database/latency" ||
		event.Status != storage.AlertEventFiring || event.Title != "Database latency" ||
		!event.StartedAt.Equal(wantStarted) {
		t.Errorf("normalised event = %+v", event)
	}
	if event.Labels["severity"] != "critical" || event.Labels["region"] != "eu-central-1" {
		t.Errorf("labels = %#v", event.Labels)
	}
	if event.GeneratorURL != "https://monitor.example/alerts/42" {
		t.Errorf("source URL = %q", event.GeneratorURL)
	}
}

func TestAdapterRejectsStructurallyInvalidJSON(t *testing.T) {
	t.Parallel()

	valid := `"eventId":"evt-42","status":"firing","title":"Alert",` +
		`"severity":"warning","startedAt":"2026-08-28T07:00:00Z",` +
		`"deduplicationKey":"alert-42"`
	tests := map[string]string{
		"unknown field":         `{` + valid + `,"organization":"another-org"}`,
		"duplicate field":       `{` + valid + `,"eventId":"replacement"}`,
		"trailing value":        `{` + valid + `} {}`,
		"invalid utf8":          `{` + valid + `,"annotations":{"note":"` + string([]byte{0xff}) + `"}}`,
		"labels are null":       `{` + valid + `,"labels":null}`,
		"labels are not object": `{` + valid + `,"labels":[]}`,
		"source URL is null":    `{` + valid + `,"sourceUrl":null}`,
		"nul in title":          `{` + strings.Replace(valid, `"title":"Alert"`, `"title":"Alert\u0000"`, 1) + `}`,
	}
	for name, body := range tests {
		name, body := name, body
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := (Adapter{}).Normalise([]byte(body)); err == nil {
				t.Fatal("Normalise() error = nil, want permanent schema rejection")
			}
		})
	}
}

func TestAdapterEnforcesCanonicalSchema(t *testing.T) {
	t.Parallel()

	base := `"eventId":"evt-42","status":"firing","title":"Alert",` +
		`"severity":"warning","startedAt":"2026-08-28T07:00:00Z",` +
		`"deduplicationKey":"alert-42"`
	tests := map[string]string{
		"missing event id":          `{"status":"firing","title":"Alert","severity":"warning","startedAt":"2026-08-28T07:00:00Z","deduplicationKey":"alert-42"}`,
		"empty trimmed title":       `{` + strings.Replace(base, `"title":"Alert"`, `"title":"   "`, 1) + `}`,
		"unknown status":            `{` + strings.Replace(base, `"status":"firing"`, `"status":"pending"`, 1) + `}`,
		"unknown severity":          `{` + strings.Replace(base, `"severity":"warning"`, `"severity":"urgent"`, 1) + `}`,
		"resolved without end":      `{` + strings.Replace(base, `"status":"firing"`, `"status":"resolved"`, 1) + `}`,
		"firing with end":           `{` + base + `,"resolvedAt":"2026-08-28T08:00:00Z"}`,
		"end before start":          `{` + strings.Replace(base, `"status":"firing"`, `"status":"resolved"`, 1) + `,"resolvedAt":"2026-08-28T06:00:00Z"}`,
		"relative source URL":       `{` + base + `,"sourceUrl":"/alerts/42"}`,
		"too many labels":           `{` + base + `,"labels":` + stringMapJSON(33, "k", "v") + `}`,
		"label key too long":        `{` + base + `,"labels":{"` + strings.Repeat("k", 65) + `":"v"}}`,
		"annotation value too long": `{` + base + `,"annotations":{"note":"` + strings.Repeat("v", 2049) + `"}}`,
	}
	for name, body := range tests {
		name, body := name, body
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := (Adapter{}).Normalise([]byte(body)); err == nil {
				t.Fatal("Normalise() error = nil, want schema rejection")
			}
		})
	}
}

func stringMapJSON(entries int, keyPrefix, value string) string {
	var body strings.Builder
	body.WriteByte('{')
	for index := 0; index < entries; index++ {
		if index > 0 {
			body.WriteByte(',')
		}
		fmt.Fprintf(&body, "%q:%q", fmt.Sprintf("%s%d", keyPrefix, index), value)
	}
	body.WriteByte('}')
	return body.String()
}
