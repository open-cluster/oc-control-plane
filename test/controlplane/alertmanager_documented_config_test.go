package controlplane

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	intake "github.com/open-cluster/oc-control-plane/internal/webhooks"
)

const (
	alertmanagerDocPage = "../../docs/integrations/alerting/alertmanager.mdx"

	alertmanagerDocOrigin = "https://oc.example.com"

	idleReceiver = "the-customers-existing-default"
)

type deployment struct {
	integration string
	secret      string
	origin      string
}

func documentedReceiver(t *testing.T) string {
	t.Helper()

	page, err := os.ReadFile(filepath.FromSlash(alertmanagerDocPage))
	if err != nil {
		t.Fatalf("reading the documentation page: %v", err)
	}

	var receivers []string
	lines := strings.Split(strings.ReplaceAll(string(page), "\r\n", "\n"), "\n")
	for index := 0; index < len(lines); index++ {
		if strings.TrimSpace(lines[index]) != "```yaml" {
			continue
		}
		indent := lines[index][:len(lines[index])-len(strings.TrimLeft(lines[index], " \t"))]
		var block []string
		for index++; index < len(lines) && strings.TrimSpace(lines[index]) != "```"; index++ {
			block = append(block, strings.TrimPrefix(lines[index], indent))
		}
		if joined := strings.Join(block, "\n"); strings.Contains(joined, "webhook_configs") {
			receivers = append(receivers, joined)
		}
	}

	if len(receivers) != 1 {
		t.Fatalf("%s carries %d webhook receiver configurations, want exactly 1: this gate "+
			"runs the configuration the page publishes and must never fall back to a copy",
			alertmanagerDocPage, len(receivers))
	}
	return receivers[0]
}

func documentedConfiguration(t *testing.T, where deployment) string {
	t.Helper()

	documented := documentedReceiver(t)
	for _, placeholder := range []string{
		"<integration-id>", "<webhook-secret>", alertmanagerDocOrigin,
	} {
		if !strings.Contains(documented, placeholder) {
			t.Fatalf("the documented configuration no longer carries %q, so this gate can no "+
				"longer substitute what a customer substitutes:\n%s", placeholder, documented)
		}
	}
	substituted := strings.NewReplacer(
		"<integration-id>", where.integration,
		"<webhook-secret>", where.secret,
		alertmanagerDocOrigin, where.origin,
	).Replace(documented)

	assertDocumentedReceiver(t, substituted, where)
	return withCustomerRootRoute(t, substituted)
}

func assertDocumentedReceiver(t *testing.T, configuration string, where deployment) {
	t.Helper()

	var documented struct {
		Receivers []struct {
			Name           string `yaml:"name"`
			WebhookConfigs []struct {
				URL          string `yaml:"url"`
				SendResolved *bool  `yaml:"send_resolved"`
				HTTPConfig   struct {
					HTTPHeaders map[string]struct {
						Secrets []string `yaml:"secrets"`
					} `yaml:"http_headers"`
				} `yaml:"http_config"`
			} `yaml:"webhook_configs"`
		} `yaml:"receivers"`
		Route struct {
			Routes []struct {
				Receiver string `yaml:"receiver"`
			} `yaml:"routes"`
		} `yaml:"route"`
	}
	if err := yaml.Unmarshal([]byte(configuration), &documented); err != nil {
		t.Fatalf("the documented configuration is not YAML: %v\n%s", err, configuration)
	}

	var name string
	var webhooks int
	for _, receiver := range documented.Receivers {
		for _, webhook := range receiver.WebhookConfigs {
			webhooks++
			name = receiver.Name
			want := where.origin + "/webhooks/v1/integrations/" + where.integration + "/alert-events"
			if webhook.URL != want {
				t.Errorf("the documented webhook url is %q, want %q; the page must point a "+
					"customer at the endpoint intake actually serves", webhook.URL, want)
			}
			if webhook.SendResolved == nil || !*webhook.SendResolved {
				t.Error("the documented receiver does not set send_resolved: true, so a " +
					"customer's resolved alerts would never arrive and their incident list " +
					"would never close")
			}
			header, ok := webhook.HTTPConfig.HTTPHeaders[intake.TokenHeader]
			if !ok {
				t.Fatalf("the documented receiver sets no %s header, so every delivery a "+
					"customer makes would be refused", intake.TokenHeader)
			}
			if len(header.Secrets) != 1 || header.Secrets[0] != where.secret {
				t.Errorf("the documented %s header carries %v, want the webhook secret",
					intake.TokenHeader, header.Secrets)
			}
		}
	}
	if webhooks != 1 {
		t.Fatalf("the documented configuration carries %d webhook configs, want exactly 1",
			webhooks)
	}

	routed := false
	for _, route := range documented.Route.Routes {
		routed = routed || route.Receiver == name
	}
	if !routed {
		t.Fatalf("the documented route does not send anything to the %q receiver, so a "+
			"customer who pasted this page would receive nothing", name)
	}
}

func withCustomerRootRoute(t *testing.T, documented string) string {
	t.Helper()

	lines := strings.Split(documented, "\n")
	withRoot := insertAfterKey(t, lines, "route:", []string{
		"  receiver: " + idleReceiver,
		"  group_by: ['alertname']",
		"  group_wait: 1s",
		"  group_interval: 1s",
	})
	return strings.Join(insertAfterKey(t, withRoot, "receivers:", []string{
		"  - name: " + idleReceiver,
	}), "\n")
}

func insertAfterKey(t *testing.T, lines []string, key string, inserted []string) []string {
	t.Helper()

	for index, line := range lines {
		if line != key {
			continue
		}
		grown := make([]string, 0, len(lines)+len(inserted))
		grown = append(grown, lines[:index+1]...)
		grown = append(grown, inserted...)
		return append(grown, lines[index+1:]...)
	}
	t.Fatalf("the documented configuration has no top-level %q to attach to:\n%s",
		key, strings.Join(lines, "\n"))
	return nil
}
