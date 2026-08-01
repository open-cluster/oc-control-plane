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

// The model boundary a deployment is given. It is optional, because an instance that serves
// relays and takes no investigations needs none; a file that was NAMED and cannot be read is a
// refusal to start, because an operator who pointed at one and got a control plane reasoning about
// nothing would have no way to tell that from one they never configured.
func TestLoad_ModelTranscriptIsOptionalAndReadFromTheFileItNames(t *testing.T) {
	t.Parallel()

	environment := validEnvironment(t)
	cfg, err := config.Load(lookupFrom(environment))
	if err != nil {
		t.Fatalf("a configuration with no transcript must load: %v", err)
	}
	if len(cfg.ModelTranscript) != 0 {
		t.Errorf("no transcript was named, got %d bytes", len(cfg.ModelTranscript))
	}

	path := filepath.Join(t.TempDir(), "transcript.json")
	if err = os.WriteFile(path, []byte(`{"key":{"model":"recorded"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	environment[config.EnvModelTranscriptFile] = path

	cfg, err = config.Load(lookupFrom(environment))
	if err != nil {
		t.Fatalf("a named transcript must load: %v", err)
	}
	if !strings.Contains(string(cfg.ModelTranscript), `"recorded"`) {
		t.Errorf("the transcript was not read, got %q", string(cfg.ModelTranscript))
	}
}

func TestLoad_AMissingModelTranscriptIsRefusedByName(t *testing.T) {
	t.Parallel()

	environment := validEnvironment(t)
	environment[config.EnvModelTranscriptFile] = filepath.Join(t.TempDir(), "absent.json")

	_, err := config.Load(lookupFrom(environment))
	if err == nil {
		t.Fatal("a transcript that was named and cannot be read must refuse to start")
	}
	if !strings.Contains(err.Error(), config.EnvModelTranscriptFile) {
		t.Errorf("the error must name the variable, got %q", err)
	}
}
