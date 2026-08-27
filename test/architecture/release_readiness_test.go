package gates_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommandEntrypointOwnsProductionCodeAndIntegrationTestsLiveUnderTest(t *testing.T) {
	t.Parallel()

	commandFiles, err := os.ReadDir(filepath.Join(moduleRoot, "cmd", "controlplane"))
	if err != nil {
		t.Fatalf("reading the control-plane executable directory: %v", err)
	}
	for _, file := range commandFiles {
		if strings.HasSuffix(file.Name(), "_test.go") {
			t.Errorf("control-plane executable directory contains integration test %q; move it to test/controlplane", file.Name())
		}
	}
	for _, path := range []string{"test/controlplane/main_test.go", "test/controlplane/eval_test.go"} {
		if _, err := os.Stat(filepath.Join(moduleRoot, filepath.FromSlash(path))); err != nil {
			t.Errorf("the dedicated control-plane test suite requires %s: %v", path, err)
		}
	}
}

func TestContinuousIntegrationReusesMinimalLocalVerificationTargets(t *testing.T) {
	t.Parallel()

	workflow, err := os.ReadFile(filepath.Join(moduleRoot, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("reading the continuous integration workflow: %v", err)
	}
	for _, target := range []string{"run: make vuln", "run: make licenses"} {
		if !strings.Contains(string(workflow), target) {
			t.Errorf("continuous integration must reuse the local verification target %q", target)
		}
	}
	makefile, err := os.ReadFile(filepath.Join(moduleRoot, "Makefile"))
	if err != nil {
		t.Fatalf("reading the local verification targets: %v", err)
	}
	if strings.Contains(string(makefile), "\ncover:") {
		t.Error("the minimal verification Makefile retains an unused coverage-artifact target")
	}
}

func TestReleaseReadinessIncludesCommunityAndDeploymentContracts(t *testing.T) {
	t.Parallel()
	for _, path := range []string{
		"CONTRIBUTING.md", "CODE_OF_CONDUCT.md", "SECURITY.md", "SUPPORT.md",
		"GOVERNANCE.md", "MAINTAINERS.md", "ROADMAP.md", "ARCHITECTURE.md",
		".dockerignore", ".github/PULL_REQUEST_TEMPLATE.md", ".github/ISSUE_TEMPLATE/bug_report.yml",
		".github/ISSUE_TEMPLATE/feature_request.yml", ".github/dependabot.yml",
		"deploy/compose/compose.yaml", "deploy/helm/opencluster/Chart.yaml",
	} {
		if _, err := os.Stat(filepath.Join(moduleRoot, filepath.FromSlash(path))); err != nil {
			t.Errorf("release readiness requires %s: %v", path, err)
		}
	}
	for path, markers := range map[string][]string{
		".github/dependabot.yml":   {"package-ecosystem: gomod", "package-ecosystem: github-actions", "interval: weekly"},
		".github/workflows/ci.yml": {"govulncheck", "go-licenses", "gitleaks"},
		".dockerignore":            {"**/*.dsn", "**/*.key", "**/*.pem", "**/.secrets/"},
	} {
		content, err := os.ReadFile(filepath.Join(moduleRoot, filepath.FromSlash(path)))
		if err != nil {
			t.Errorf("reading release contract %s: %v", path, err)
			continue
		}
		for _, marker := range markers {
			if !strings.Contains(string(content), marker) {
				t.Errorf("release contract %s must include %q", path, marker)
			}
		}
	}
}

func TestReleaseLicensePermitsOpenSourceSelfHosting(t *testing.T) {
	t.Parallel()
	license, err := os.ReadFile(filepath.Join(moduleRoot, "LICENSE"))
	if err != nil {
		t.Fatalf("reading the distribution license: %v", err)
	}
	for _, marker := range []string{"Apache License", "Version 2.0, January 2004"} {
		if !strings.Contains(string(license), marker) {
			t.Errorf("the control plane must ship the Apache-2.0 license: missing %q", marker)
		}
	}
	for _, path := range []string{"README.md", "CONTRIBUTING.md", "GOVERNANCE.md", "ROADMAP.md"} {
		content, readErr := os.ReadFile(filepath.Join(moduleRoot, path))
		if readErr != nil {
			t.Errorf("reading open-source release document %s: %v", path, readErr)
			continue
		}
		if strings.Contains(strings.ToLower(string(content)), "proprietary") {
			t.Errorf("open-source release document %s still describes the control plane as proprietary", path)
		}
	}
}

func TestSelfHostedExamplesEnableOperatorAndWebhookRoutesOnOneListener(t *testing.T) {
	t.Parallel()

	for _, path := range []string{
		"deploy/compose/opencluster.yaml",
		"deploy/helm/opencluster/templates/configmap.yaml",
	} {
		content, err := os.ReadFile(filepath.Join(moduleRoot, filepath.FromSlash(path)))
		if err != nil {
			t.Errorf("reading deployment configuration %s: %v", path, err)
			continue
		}
		for _, routeGroup := range []string{"address:", "public_url:"} {
			if !strings.Contains(string(content), routeGroup) {
				t.Errorf("deployment configuration %s must enable %s on its shared HTTP listener", path, routeGroup)
			}
		}
	}
}

func TestSelfHostedReleaseIncludesSameOriginInvestigationConsole(t *testing.T) {
	t.Parallel()

	for path, markers := range map[string][]string{
		"web/index.html": {"OpenCluster", "bootstrap", "sign-in", "investigation"},
		"web/app.js":     {"/report", "/activity", "/sources", "/cancel", "credentials: 'same-origin'"},
	} {
		content, err := os.ReadFile(filepath.Join(moduleRoot, filepath.FromSlash(path)))
		if err != nil {
			t.Errorf("reading the self-hosted browser asset %s: %v", path, err)
			continue
		}
		for _, marker := range markers {
			if !strings.Contains(string(content), marker) {
				t.Errorf("self-hosted browser asset %s must include %q", path, marker)
			}
		}
	}
}

func TestOpenSourceBuildIncludesItsProtocolAndBrowserWithoutPrivateCredentials(t *testing.T) {
	t.Parallel()

	for path, required := range map[string][]string{
		".dockerignore":             {"!web/", "!web/**", "!third_party/relay-protocol/"},
		"go.mod":                    {"=> ./third_party/relay-protocol"},
		"test/e2e/go.mod":           {"=> ../../third_party/relay-protocol"},
		"deploy/compose/Dockerfile": {"COPY third_party/relay-protocol ./third_party/relay-protocol"},
	} {
		content, err := os.ReadFile(filepath.Join(moduleRoot, filepath.FromSlash(path)))
		if err != nil {
			t.Errorf("reading open-source build input %s: %v", path, err)
			continue
		}
		for _, marker := range required {
			if !strings.Contains(string(content), marker) {
				t.Errorf("open-source build input %s must include %q", path, marker)
			}
		}
	}
	for _, path := range []string{"deploy/compose/Dockerfile", "deploy/compose/compose.yaml"} {
		content, err := os.ReadFile(filepath.Join(moduleRoot, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"type=ssh", "github_known_hosts", "GOPRIVATE"} {
			if strings.Contains(string(content), forbidden) {
				t.Errorf("open-source build input %s still requires private access through %q",
					path, forbidden)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(moduleRoot, "third_party", "relay-protocol", "LICENSE")); err != nil {
		t.Errorf("the bundled Relay protocol must retain its Apache-2.0 license: %v", err)
	}
}

func TestDeploymentExamplesOfferPinnedTLSRelayAndBehavioralCIGates(t *testing.T) {
	t.Parallel()

	for path, markers := range map[string][]string{
		"deploy/compose/compose.yaml": {
			"OC_RELAY_ADDRESS", "OC_RELAY_SPKI_PINS", "relay-tls:", "profiles: [relay]",
		},
		"deploy/compose/relay-nginx.conf":                   {"ssl_certificate", "ssl_certificate_key", "grpc_pass grpc://control-plane:8444"},
		"deploy/helm/opencluster/values.yaml":               {"relay:", "spkiPins:", "existingSecret:", "externalPort:"},
		"deploy/helm/opencluster/templates/configmap.yaml":  {"relay:", "spki_pins:", "nginx.conf:"},
		"deploy/helm/opencluster/templates/deployment.yaml": {"name: relay-tls", "name: relay-certificates"},
		"deploy/helm/opencluster/templates/service.yaml":    {"name: relay-tls"},
		".github/workflows/ci.yml":                          {"make deploy-verify", "needs.relay-availability.outputs.available"},
		"Makefile":                                          {"deploy-verify:", "docker compose", "helm lint", "helm template", "docker build", "verify: lint build test vuln licenses deploy-verify"},
		"README.md":                                         {"OPENCLUSTER_RELAY_SPKI_PINS", "--profile relay", "relay.enabled=true"},
	} {
		content, err := os.ReadFile(filepath.Join(moduleRoot, filepath.FromSlash(path)))
		if err != nil {
			t.Errorf("reading Relay deployment contract %s: %v", path, err)
			continue
		}
		for _, marker := range markers {
			if !strings.Contains(string(content), marker) {
				t.Errorf("Relay deployment contract %s must include %q", path, marker)
			}
		}
	}
}

func TestSecurityDocumentationDisclosesLiveReadsScopedRepliesAndCancellation(t *testing.T) {
	t.Parallel()

	for path, markers := range map[string][]string{
		"docs/security/security-model.mdx":                {"container logs", "thread replies", "cancelled"},
		"docs/security/credentials-and-data-access.mdx":   {"container logs", "thread replies", "model provider"},
		"docs/integrations/collaboration/slack.mdx":       {"chat:write", "app_mentions:read", "groups:history"},
		"docs/investigations/investigate-an-incident.mdx": {"cancelled", "Cancel"},
	} {
		content, err := os.ReadFile(filepath.Join(moduleRoot, filepath.FromSlash(path)))
		if err != nil {
			t.Errorf("reading security disclosure %s: %v", path, err)
			continue
		}
		for _, marker := range markers {
			if !strings.Contains(string(content), marker) {
				t.Errorf("security disclosure %s must include %q", path, marker)
			}
		}
	}
	for path, forbidden := range map[string][]string{
		"docs/security/security-model.mdx":                 {"does not write to Slack", "does not hold cluster credentials or perform live investigation reads"},
		"docs/getting-started/how-investigations-work.mdx": {"does not provide live pod, event, or log reads"},
	} {
		content, err := os.ReadFile(filepath.Join(moduleRoot, filepath.FromSlash(path)))
		if err != nil {
			t.Errorf("reading security disclosure %s: %v", path, err)
			continue
		}
		for _, marker := range forbidden {
			if strings.Contains(string(content), marker) {
				t.Errorf("security disclosure %s still misrepresents product behavior through %q", path, marker)
			}
		}
	}
}
