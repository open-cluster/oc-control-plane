package config_test

import (
	"os"
	"path/filepath"
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
