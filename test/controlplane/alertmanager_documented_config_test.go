package controlplane

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	intake "github.com/open-cluster/oc-control-plane/internal/webhooks"
)

// The documentation page read as configuration.
//
// The reason to run the YAML off the page, rather than keep a copy of it beside a test,
// is that the failure this gate exists to catch is exactly documentation drift: a test
// carrying its own copy passes forever while the page rots.

const (

	// alertmanagerDocPage is the page a customer copies from. Its YAML is this test's input.
	alertmanagerDocPage = "../../docs/integrations/alerting/alertmanager.mdx"

	// alertmanagerDocOrigin is the example intake origin the page publishes, and one of the
	// three deployment-specific values a customer replaces.
	alertmanagerDocOrigin = "https://oc.example.com"

	// idleReceiver stands in for the default receiver a customer's existing configuration
	// already has. The documented fragment deliberately carries no root receiver — it says
	// "add the receiver, then route alerts to it" — and Alertmanager refuses a configuration
	// without one.
	idleReceiver = "the-customers-existing-default"
)

// deployment is the three values the page leaves as placeholders — everything about the
// configuration that is specific to where it runs. They travel together because they are
// substituted together, and because three bare strings in a row is an argument order
// waiting to be got wrong.
type deployment struct {
	integration string
	secret      string
	origin      string
}

// documentedReceiver returns the one fenced YAML block on the documentation page that
// configures a webhook receiver, dedented to column zero.
//
// Extraction is asserted rather than attempted. A page that no longer carries exactly one
// such block fails the build, because the alternative — quietly falling back to a copy of
// the YAML held here — is the failure this whole gate exists to prevent.
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
		// The fence sits inside an MDX step, so the block is indented. That indent is the
		// page's layout rather than the configuration's, and carrying it into the YAML would
		// make every documented line a child of nothing.
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

// documentedConfiguration renders the configuration a customer following the page would
// actually be running: the documented lines with the three deployment-specific values
// substituted, plus the root route their existing configuration already has.
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
	// Three substitutions, and only three. The scheme becomes plain HTTP with the origin
	// because terminating TLS is not what this gate proves; everything else is the page's
	// own text.
	substituted := strings.NewReplacer(
		"<integration-id>", where.integration,
		"<webhook-secret>", where.secret,
		alertmanagerDocOrigin, where.origin,
	).Replace(documented)

	assertDocumentedReceiver(t, substituted, where)
	return withCustomerRootRoute(t, substituted)
}

// assertDocumentedReceiver checks that what was extracted is a usable receiver rather than
// prose that happened to mention a webhook. Each failure here names a way the page could rot
// while still looking like a configuration.
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
			want := where.origin + "/intake/v1/integrations/" + where.integration + "/signals"
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

// withCustomerRootRoute adds what the customer's own configuration supplies around the
// documented fragment, and nothing else.
//
// Every documented line is carried through BYTE FOR BYTE — lines are inserted, never
// rewritten. Parsing and re-emitting the document would hand Alertmanager the YAML library's
// rendering rather than the page's, and then the quoting, the anchors and the block scalars
// on the page would be outside the gate while it still reported green.
//
// What is inserted is a root receiver, which Alertmanager requires and the fragment
// deliberately omits because it says "add the receiver, then route alerts to it"; grouping by
// alert name; and group timings short enough that this gate does not spend Alertmanager's
// default thirty-second group wait on every alert. All three live on the ROOT route, which is
// the customer's, never on the documented one.
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

// insertAfterKey puts lines directly beneath a top-level mapping key, leaving every other
// line untouched. A key the page no longer publishes at the top level is fatal: silently
// producing a configuration missing what a customer's own file supplies would make the gate
// fail for a reason that is not the customer's.
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
