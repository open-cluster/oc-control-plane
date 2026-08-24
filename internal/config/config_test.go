package config_test

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/config"
)

// lookupFrom builds an environment lookup over a map, so no test touches the real
// process environment and tests stay parallel-safe.
func lookupFrom(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

// dsnFile writes a DSN to a temp file and returns its path. Placement DSNs carry a
// password, so they are referenced by path and never by environment value.
func dsnFile(t *testing.T, dsn string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dsn")
	if err := os.WriteFile(path, []byte(dsn+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func validEnvironment(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"OC_HTTP_ADDRESS":          "127.0.0.1:8080",
		"OC_PLACEMENTS":            "shared=" + dsnFile(t, "postgres://user:pw@localhost:5432/shared"),
		"OC_PLACEMENT_ASSIGNMENTS": "org-a=shared,org-b=shared",
	}
}

func TestLoadProcess_YAMLThenEnvironmentThenCLI(t *testing.T) {
	t.Parallel()

	dsn := dsnFile(t, "postgres://user:pw@localhost:5432/shared")
	path := filepath.Join(t.TempDir(), "opencluster.yaml")
	document := strings.Join([]string{
		"server:",
		"  address: 127.0.0.1:8080",
		"  shutdown_timeout: 45s",
		"database:",
		"  placements:",
		"    shared:",
		"      dsn_file: " + dsn,
		"  assignments:",
		"    org-a: shared",
		"telemetry:",
		"  service_name: from-file",
	}, "\n")
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.LoadProcess(
		[]string{"--config", path, "--server-address", "127.0.0.1:9090"},
		lookupFrom(map[string]string{config.EnvServiceName: "from-environment"}),
	)
	if err != nil {
		t.Fatalf("load process configuration: %v", err)
	}

	if cfg.HTTPAddress != "127.0.0.1:9090" {
		t.Errorf("server address = %q, want the CLI override", cfg.HTTPAddress)
	}
	if cfg.ServiceName != "from-environment" {
		t.Errorf("service name = %q, want the environment override", cfg.ServiceName)
	}
	if cfg.ShutdownTimeout != 45*time.Second {
		t.Errorf("shutdown timeout = %v, want the YAML value", cfg.ShutdownTimeout)
	}
	if cfg.Placements["shared"] != "postgres://user:pw@localhost:5432/shared" {
		t.Error("the YAML placement DSN was not resolved from its file")
	}
	if cfg.Assignments["org-a"] != "shared" {
		t.Errorf("assignments = %v", cfg.Assignments)
	}
}

func TestLoadProcess_UnknownYAMLFieldIsRefused(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "opencluster.yaml")
	if err := os.WriteFile(path, []byte("server:\n  adress: 127.0.0.1:8080\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := config.LoadProcess([]string{"--config", path}, lookupFrom(nil))
	if err == nil {
		t.Fatal("an unknown YAML field must be refused")
	}
	if !strings.Contains(err.Error(), "adress") {
		t.Errorf("the error must name the unknown field, got %q", err)
	}
}

func TestLoadProcess_MultipleYAMLDocumentsAreRefused(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "opencluster.yaml")
	if err := os.WriteFile(path, []byte(strings.Join([]string{
		"server:",
		"  address: 127.0.0.1:8080",
		"---",
		"server:",
		"  address: 127.0.0.1:9090",
	}, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := config.LoadProcess([]string{"--config", path}, lookupFrom(nil))
	if err == nil {
		t.Fatal("a second YAML document must be refused")
	}
	if !strings.Contains(err.Error(), "one YAML document") {
		t.Errorf("the error must explain the one-document contract, got %q", err)
	}
}

func TestLoadProcess_NestedYAMLCoversCurrentConfiguration(t *testing.T) {
	t.Parallel()

	secretFile := func(name, value string) string {
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
		return filepath.ToSlash(path)
	}
	dsn := secretFile("dsn", "postgres://user:pw@localhost:5432/shared")
	operatorToken := secretFile("operator-token", strings.Repeat("a", 32))
	sealingKey := secretFile("sealing-key", strings.Repeat("k", 32))
	slackClientSecret := secretFile("slack-client-secret", "slack-client-value")
	slackSigningSecret := secretFile("slack-signing-secret", "slack-signing-value")
	gitHubAppKey := secretFile("github-app-key", appKeyBytes)
	gitHubClientSecret := secretFile("github-client-secret", "github-client-value")
	modelKey := secretFile("model-key", "model-key-value")
	pin := base64.StdEncoding.EncodeToString(make([]byte, sha256.Size))

	path := filepath.Join(t.TempDir(), "opencluster.yaml")
	document := strings.NewReplacer(
		"$DSN", dsn,
		"$OPERATOR_TOKEN", operatorToken,
		"$SEALING_KEY", sealingKey,
		"$SLACK_CLIENT_SECRET", slackClientSecret,
		"$SLACK_SIGNING_SECRET", slackSigningSecret,
		"$GITHUB_APP_KEY", gitHubAppKey,
		"$GITHUB_CLIENT_SECRET", gitHubClientSecret,
		"$MODEL_KEY", modelKey,
		"$RELAY_PIN", pin,
	).Replace(strings.TrimSpace(`
server:
  address: 127.0.0.1:8080
  operator_address: 127.0.0.1:8081
  intake_address: 127.0.0.1:8082
  public_url: http://localhost:8081
  console_url: http://localhost:3000
  intake_public_url: http://localhost:8082
  allowed_origins: [http://localhost:3000]
  shutdown_timeout: 30s
database:
  placements:
    shared:
      dsn_file: $DSN
  default_placement: shared
authentication:
  bootstrap_token_file: $OPERATOR_TOKEN
  bootstrap_organization: org-a
  bootstrap_role: editor
  sealing_key_file: $SEALING_KEY
relay:
  address: 127.0.0.1:8443
  spki_pins: [$RELAY_PIN]
  inventory_interval: 10m
  minimum_version: v1.2.3
telemetry:
  service_name: yaml-plane
  otlp_endpoint: collector.example:4317
providers:
  slack:
    api_url: http://localhost:18080
    client_id: slack-client
    client_secret_file: $SLACK_CLIENT_SECRET
    signing_secret_file: $SLACK_SIGNING_SECRET
    agent_organizations: [org-a, org-b]
  github:
    app_id: "12345"
    app_private_key_file: $GITHUB_APP_KEY
    api_url: http://localhost:18081
    app_slug: opencluster
    client_id: github-client
    client_secret_file: $GITHUB_CLIENT_SECRET
    web_url: http://localhost:18082
model:
  provider: anthropic
  name: model-test
  api_key_file: $MODEL_KEY
  effort: medium
  consented_providers: [anthropic]
  base_url: http://localhost:18083
  spend_ceiling_cents: 123
  context_window: 100000
investigations:
  window_lead: 3h
  max_tool_runs: 31
  max_turns: 21
conversations:
  enabled: true
  max_concurrent_investigations: 5
  max_waiting_investigations: 17
  context_threshold_percent: 60
change_ledger:
  retention_days: 120
`))
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.LoadProcess([]string{"--config", path}, lookupFrom(nil))
	if err != nil {
		t.Fatalf("load process configuration: %v", err)
	}

	if cfg.OperatorAddress != "127.0.0.1:8081" || cfg.IntakeAddress != "127.0.0.1:8082" {
		t.Errorf("server surfaces = operator %q intake %q", cfg.OperatorAddress, cfg.IntakeAddress)
	}
	if cfg.OperatorPublicURL != "http://localhost:8081" ||
		cfg.OperatorConsoleURL != "http://localhost:3000" ||
		cfg.IntakePublicURL != "http://localhost:8082" {
		t.Errorf("public URLs = %q %q %q", cfg.OperatorPublicURL,
			cfg.OperatorConsoleURL, cfg.IntakePublicURL)
	}
	if len(cfg.OperatorTokenDigest) != sha256.Size || cfg.OperatorTokenOrganization != "org-a" ||
		cfg.OperatorTokenRole != "editor" || len(cfg.SealingKey) != 32 {
		t.Error("the nested authentication configuration was not applied")
	}
	if cfg.RelayAddress != "127.0.0.1:8443" || len(cfg.RelaySPKIPins) != 1 ||
		cfg.InventoryInterval != 10*time.Minute || cfg.MinimumRelayVersion != "v1.2.3" {
		t.Error("the nested Relay configuration was not applied")
	}
	if cfg.SlackClientSecret != "slack-client-value" ||
		cfg.SlackSigningSecret != "slack-signing-value" ||
		!cfg.SlackAgentLiveFor("org-b") {
		t.Error("the nested Slack configuration was not applied")
	}
	if cfg.GitHubAppID != "12345" || cfg.GitHubClientSecret != "github-client-value" {
		t.Error("the nested GitHub configuration was not applied")
	}
	if cfg.ModelProvider != "anthropic" || cfg.ModelKey != "model-key-value" ||
		cfg.ModelSpendCeilingCents != 123 || cfg.ModelContextWindow != 100000 {
		t.Error("the nested model configuration was not applied")
	}
	if cfg.InvestigationWindowLead != 3*time.Hour || cfg.InvestigationMaxToolRuns != 31 ||
		cfg.InvestigationMaxTurns != 21 {
		t.Error("the nested Investigation configuration was not applied")
	}
	if !cfg.ConversationsEnabled || cfg.OrgConcurrentInvestigations != 5 ||
		cfg.OrgWaitingInvestigations != 17 || cfg.ContextThresholdPercent != 60 {
		t.Error("the nested Conversation configuration was not applied")
	}
	if cfg.ChangeLedgerRetentionDays != 120 {
		t.Errorf("change ledger retention = %d", cfg.ChangeLedgerRetentionDays)
	}
}

func TestLoadProcess_StructuredYAMLPreservesCommas(t *testing.T) {
	t.Parallel()

	directory := filepath.Join(t.TempDir(), "dsn,files")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	dsnPath := filepath.Join(directory, "shared")
	if err := os.WriteFile(dsnPath, []byte("postgres://database.example/opencluster"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "opencluster.yaml")
	document := fmt.Sprintf(`
server:
  address: ":8080"
database:
  placements:
    shared:
      dsn_file: %q
  assignments:
    "org,west": shared
providers:
  slack:
    agent_organizations: ["org,west"]
`, dsnPath)
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.LoadProcess([]string{"--config", path}, lookupFrom(nil))
	if err != nil {
		t.Fatalf("LoadProcess() error = %v", err)
	}
	if got := cfg.Placements["shared"]; got != "postgres://database.example/opencluster" {
		t.Errorf("Placements[shared] = %q", got)
	}
	if got := cfg.Assignments["org,west"]; got != "shared" {
		t.Errorf("Assignments[org,west] = %q", got)
	}
	if got := cfg.SlackAgentOrganizations; !reflect.DeepEqual(got, []string{"org,west"}) {
		t.Errorf("SlackAgentOrganizations = %#v", got)
	}
}

func TestLoad_LegacyListPreservesBareQuotes(t *testing.T) {
	t.Parallel()

	environment := validEnvironment(t)
	environment[config.EnvSlackAgentOrganizations] = `org"west,org-east`

	cfg, err := config.Load(lookupFrom(environment))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := []string{`org"west`, "org-east"}
	if !reflect.DeepEqual(cfg.SlackAgentOrganizations, want) {
		t.Errorf("SlackAgentOrganizations = %#v, want %#v",
			cfg.SlackAgentOrganizations, want)
	}
}

func TestLoad_ValidEnvironmentAppliesDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(lookupFrom(validEnvironment(t)))
	if err != nil {
		t.Fatalf("valid configuration must load: %v", err)
	}

	if cfg.HTTPAddress != "127.0.0.1:8080" {
		t.Errorf("HTTPAddress = %q", cfg.HTTPAddress)
	}
	if cfg.ShutdownTimeout != 15*time.Second {
		t.Errorf("ShutdownTimeout default = %v, want 15s", cfg.ShutdownTimeout)
	}
	if cfg.ServiceName != "oc-control-plane" {
		t.Errorf("ServiceName default = %q", cfg.ServiceName)
	}
	if cfg.OTLPEndpoint != "" {
		t.Errorf("OTLPEndpoint must default to empty (export disabled), got %q", cfg.OTLPEndpoint)
	}
}

func TestLoad_ResolvesPlacementDSNFromFile(t *testing.T) {
	t.Parallel()

	const dsn = "postgres://user:secret@localhost:5432/shared"
	environment := validEnvironment(t)
	environment["OC_PLACEMENTS"] = "shared=" + dsnFile(t, dsn)

	cfg, err := config.Load(lookupFrom(environment))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if got := cfg.Placements["shared"]; got != dsn {
		t.Errorf("placement DSN = %q, want the file's trimmed contents", got)
	}
}

func TestLoad_AssignmentsMapOrganizationsToPlacements(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(lookupFrom(validEnvironment(t)))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Assignments["org-a"] != "shared" || cfg.Assignments["org-b"] != "shared" {
		t.Errorf("assignments = %v", cfg.Assignments)
	}
	if len(cfg.Assignments) != 2 {
		t.Errorf("assignments must contain exactly the configured entries, got %v", cfg.Assignments)
	}
}

// Every required variable must be reported by name, so a failed start says what to fix.
func TestLoad_RequiredVariablesAreReportedByName(t *testing.T) {
	t.Parallel()

	for _, key := range []string{"OC_HTTP_ADDRESS", "OC_PLACEMENTS", "OC_PLACEMENT_ASSIGNMENTS"} {
		environment := validEnvironment(t)
		delete(environment, key)

		_, err := config.Load(lookupFrom(environment))
		if err == nil {
			t.Fatalf("%s missing must fail", key)
		}
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error for missing %s must name it, got %q", key, err)
		}
	}
}

func TestLoad_RejectsInvalidValues(t *testing.T) {
	t.Parallel()

	cases := map[string]struct{ key, value string }{
		"address without port":     {"OC_HTTP_ADDRESS", "127.0.0.1"},
		"address with scheme":      {"OC_HTTP_ADDRESS", "http://127.0.0.1:8080"},
		"address with port zero":   {"OC_HTTP_ADDRESS", "127.0.0.1:0"},
		"placement without name":   {"OC_PLACEMENTS", "=/tmp/dsn"},
		"placement without path":   {"OC_PLACEMENTS", "shared="},
		"placement malformed":      {"OC_PLACEMENTS", "shared"},
		"assignment malformed":     {"OC_PLACEMENT_ASSIGNMENTS", "org-a"},
		"assignment without org":   {"OC_PLACEMENT_ASSIGNMENTS", "=shared"},
		"negative shutdown":        {"OC_SHUTDOWN_TIMEOUT", "-1s"},
		"unparseable shutdown":     {"OC_SHUTDOWN_TIMEOUT", "soon"},
		"zero shutdown":            {"OC_SHUTDOWN_TIMEOUT", "0s"},
		"otlp endpoint with path":  {"OC_OTLP_ENDPOINT", "collector:4317/v1/traces"},
		"otlp endpoint no port":    {"OC_OTLP_ENDPOINT", "collector"},
		"blank service name":       {"OC_SERVICE_NAME", "   "},
		"assignment unknown place": {"OC_PLACEMENT_ASSIGNMENTS", "org-a=nowhere"},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			environment := validEnvironment(t)
			environment[testCase.key] = testCase.value

			_, err := config.Load(lookupFrom(environment))
			if err == nil {
				t.Fatalf("%s=%q must be refused", testCase.key, testCase.value)
			}
			if !strings.Contains(err.Error(), testCase.key) {
				t.Errorf("error must name %s, got %q", testCase.key, err)
			}
		})
	}
}

// A DSN carries a password. A failure to read or parse one must never quote the file's
// contents, or a failed start writes the credential into the log aggregator.
func TestLoad_NeverEchoesDSNContents(t *testing.T) {
	t.Parallel()

	const password = "sup3r-s3cret-canary"
	path := dsnFile(t, "postgres://user:"+password+"@localhost:5432/db")
	if err := os.Chmod(path, 0o000); err != nil {
		t.Skipf("cannot make the DSN file unreadable on this platform: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	environment := validEnvironment(t)
	environment["OC_PLACEMENTS"] = "shared=" + path

	_, err := config.Load(lookupFrom(environment))
	if err == nil {
		t.Skip("the platform allowed the read; the redaction path is not exercised")
	}
	if strings.Contains(err.Error(), password) {
		t.Fatal("the DSN file's contents must never appear in an error")
	}
}

func TestLoad_EmptyDSNFileIsRefused(t *testing.T) {
	t.Parallel()

	environment := validEnvironment(t)
	environment["OC_PLACEMENTS"] = "shared=" + dsnFile(t, "   ")

	if _, err := config.Load(lookupFrom(environment)); err == nil {
		t.Fatal("an empty DSN file must be refused")
	}
}

func TestLoad_MissingDSNFileNamesTheVariableNotTheContents(t *testing.T) {
	t.Parallel()

	environment := validEnvironment(t)
	environment["OC_PLACEMENTS"] = "shared=" + filepath.Join(t.TempDir(), "absent")

	_, err := config.Load(lookupFrom(environment))
	if err == nil {
		t.Fatal("a missing DSN file must be refused")
	}
	if !strings.Contains(err.Error(), "OC_PLACEMENTS") {
		t.Errorf("error must name OC_PLACEMENTS, got %q", err)
	}
}

func TestLoad_OptionalOverridesApply(t *testing.T) {
	t.Parallel()

	environment := validEnvironment(t)
	environment["OC_SHUTDOWN_TIMEOUT"] = "45s"
	environment["OC_SERVICE_NAME"] = "control-plane-eu"
	environment["OC_OTLP_ENDPOINT"] = "collector.observability:4317"

	cfg, err := config.Load(lookupFrom(environment))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.ShutdownTimeout != 45*time.Second {
		t.Errorf("ShutdownTimeout = %v", cfg.ShutdownTimeout)
	}
	if cfg.ServiceName != "control-plane-eu" {
		t.Errorf("ServiceName = %q", cfg.ServiceName)
	}
	if cfg.OTLPEndpoint != "collector.observability:4317" {
		t.Errorf("OTLPEndpoint = %q", cfg.OTLPEndpoint)
	}
}

func TestLoad_InvestigationKnobsDefaultAndValidate(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(lookupFrom(validEnvironment(t)))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.InvestigationMaxToolRuns != 0 || cfg.InvestigationMaxTurns != 0 {
		t.Error("unset ceilings mean the built-in defaults, spelled zero")
	}

	environment := validEnvironment(t)
	environment["OC_INVESTIGATION_MAX_TOOL_RUNS"] = "40"
	environment["OC_INVESTIGATION_MAX_TURNS"] = "25"
	cfg, err = config.Load(lookupFrom(environment))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.InvestigationMaxToolRuns != 40 || cfg.InvestigationMaxTurns != 25 {
		t.Errorf("cfg = %d %d", cfg.InvestigationMaxToolRuns, cfg.InvestigationMaxTurns)
	}

	for key, value := range map[string]string{
		"OC_INVESTIGATION_MAX_TOOL_RUNS": "0",
		"OC_INVESTIGATION_MAX_TURNS":     "-3",
	} {
		environment := validEnvironment(t)
		environment[key] = value
		if _, err := config.Load(lookupFrom(environment)); err == nil ||
			!strings.Contains(err.Error(), key) {
			t.Errorf("%s=%q must be refused naming the variable, got %v", key, value, err)
		}
	}
}

// appKeyBytes stands in for a GitHub App key wherever a test only needs the file to exist.
// Configuration never parses it — it checks the file is non-empty and hands the bytes on —
// so the content is arbitrary, and it is deliberately NOT a PEM header: a real one here is
// indistinguishable to a secret scanner from a leaked key, and reading history is how that
// gate works. One such line turned the whole build red for a day.
const appKeyBytes = "an app key; configuration never parses this"

// The installation flow needs three values and cannot work with two. A deployment that set
// only some of them would offer a Connect button that fails at the last step, in front of a
// customer — so it is refused here, where whoever set them is still reading.
func TestLoad_GitHubInstallationFlowIsAllThreeOrNone(t *testing.T) {
	t.Parallel()

	appKey := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(appKey, []byte(appKeyBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	secretFile := filepath.Join(t.TempDir(), "client-secret")
	if err := os.WriteFile(secretFile, []byte("shhh\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	withApp := func(t *testing.T) map[string]string {
		environment := validEnvironment(t)
		environment[config.EnvGitHubAppID] = "12345"
		environment[config.EnvGitHubAppKeyFile] = appKey
		return environment
	}

	whole := withApp(t)
	whole[config.EnvGitHubAppSlug] = "opencluster"
	whole[config.EnvGitHubClientID] = "Iv1.deployment"
	whole[config.EnvGitHubClientSecretFile] = secretFile

	cfg, err := config.Load(lookupFrom(whole))
	if err != nil {
		t.Fatalf("a whole installation flow must load: %v", err)
	}
	if cfg.GitHubAppSlug != "opencluster" || cfg.GitHubClientID != "Iv1.deployment" {
		t.Errorf("the installation flow did not load: %+v", cfg.GitHubAppSlug)
	}
	if cfg.GitHubClientSecret != "shhh" {
		t.Errorf("the client secret came from somewhere other than its file")
	}

	partial := withApp(t)
	partial[config.EnvGitHubAppSlug] = "opencluster"
	if _, err := config.Load(lookupFrom(partial)); err == nil {
		t.Error("two of three configured a connect button that cannot finish")
	}

	// A deployment that registered none is the self-hosted path and must still load.
	if cfg, err := config.Load(lookupFrom(withApp(t))); err != nil {
		t.Errorf("a deployment with an app and no installation flow must load: %v", err)
	} else if cfg.GitHubAppSlug != "" {
		t.Error("an installation flow appeared from nowhere")
	}
}

// The client secret is read from a file and never echoed, for the reason every credential
// here is: an error that quotes the file's contents is a credential in a log.
func TestLoad_GitHubClientSecretNeverAppearsInAnError(t *testing.T) {
	t.Parallel()

	appKey := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(appKey, []byte(appKeyBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	environment := validEnvironment(t)
	environment[config.EnvGitHubAppID] = "12345"
	environment[config.EnvGitHubAppKeyFile] = appKey
	environment[config.EnvGitHubAppSlug] = "opencluster"
	environment[config.EnvGitHubClientID] = "Iv1.deployment"
	environment[config.EnvGitHubClientSecretFile] = filepath.Join(t.TempDir(), "absent")

	_, err := config.Load(lookupFrom(environment))
	if err == nil {
		t.Fatal("a client secret file that does not exist must be refused")
	}
	if !strings.Contains(err.Error(), config.EnvGitHubClientSecretFile) {
		t.Errorf("the error %q does not name the variable to fix", err)
	}
}

// The Slack app is deployment configuration and follows the same rule the GitHub one
// does: variables name FILES, never secret values, and a half-configured flow is refused
// at startup rather than offered as a button that cannot finish.
func TestLoad_SlackInstallationFlowIsBothHalvesOrNeither(t *testing.T) {
	t.Parallel()

	secretFile := filepath.Join(t.TempDir(), "client-secret")
	if err := os.WriteFile(secretFile, []byte("slack-client-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	signingFile := filepath.Join(t.TempDir(), "signing-secret")
	if err := os.WriteFile(signingFile, []byte("slack-signing-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	whole := validEnvironment(t)
	whole[config.EnvSlackClientID] = "4444.5555"
	whole[config.EnvSlackClientSecretFile] = secretFile
	whole[config.EnvSlackSigningSecretFile] = signingFile

	cfg, err := config.Load(lookupFrom(whole))
	if err != nil {
		t.Fatalf("a whole slack app must load: %v", err)
	}
	if cfg.SlackClientID != "4444.5555" {
		t.Errorf("the client id did not load: %q", cfg.SlackClientID)
	}
	if cfg.SlackClientSecret != "slack-client-secret" {
		t.Error("the client secret came from somewhere other than its file")
	}
	if cfg.SlackSigningSecret != "slack-signing-secret" {
		t.Error("the signing secret came from somewhere other than its file")
	}

	partial := validEnvironment(t)
	partial[config.EnvSlackClientID] = "4444.5555"
	if _, err := config.Load(lookupFrom(partial)); err == nil {
		t.Error("half a client credential configured a connect button that cannot finish")
	}

	// A deployment that registered no Slack app is the self-hosted path: it must load,
	// and it keeps the pasted-token form.
	if cfg, err := config.Load(lookupFrom(validEnvironment(t))); err != nil {
		t.Errorf("a deployment with no slack app must load: %v", err)
	} else if cfg.SlackClientID != "" || cfg.SlackSigningSecret != "" {
		t.Error("a slack app appeared from nowhere")
	}
}

// The signing secret stands alone. It serves the events endpoint, not the connect flow, so
// a deployment may hold one without a client credential — and an air-gapped install that
// pasted a token can still receive events.
func TestLoad_SlackSigningSecretIsIndependentOfTheConnectFlow(t *testing.T) {
	t.Parallel()

	signingFile := filepath.Join(t.TempDir(), "signing-secret")
	if err := os.WriteFile(signingFile, []byte("only-the-signing-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	environment := validEnvironment(t)
	environment[config.EnvSlackSigningSecretFile] = signingFile

	cfg, err := config.Load(lookupFrom(environment))
	if err != nil {
		t.Fatalf("a signing secret without a connect flow must load: %v", err)
	}
	if cfg.SlackSigningSecret != "only-the-signing-secret" {
		t.Errorf("the signing secret did not load: %q", cfg.SlackSigningSecret)
	}
}

func TestLoad_SlackSecretsNeverAppearInAnError(t *testing.T) {
	t.Parallel()

	environment := validEnvironment(t)
	environment[config.EnvSlackClientID] = "4444.5555"
	environment[config.EnvSlackClientSecretFile] = filepath.Join(t.TempDir(), "absent")

	_, err := config.Load(lookupFrom(environment))
	if err == nil {
		t.Fatal("a client secret file that does not exist must be refused")
	}
	if !strings.Contains(err.Error(), config.EnvSlackClientSecretFile) {
		t.Errorf("the error %q does not name the variable to fix", err)
	}
}
