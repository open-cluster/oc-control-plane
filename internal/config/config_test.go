package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func secretFile(t *testing.T, value string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func lookup(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) { value, ok := values[key]; return value, ok }
}

func essentialEnvironment(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		EnvDatabaseDSNFile:   secretFile(t, "postgres://user:password@localhost/opencluster"),
		EnvOperatorTokenFile: secretFile(t, strings.Repeat("b", 32)),
		EnvSealingKeyFile:    secretFile(t, strings.Repeat("k", 32)),
		EnvModelProvider:     "anthropic", EnvModelName: "model",
		EnvModelKeyFile: secretFile(t, "model-key"),
	}
}

func TestLoadUsesSafeDefaultsAndTheEssentialOSSSurface(t *testing.T) {
	values := essentialEnvironment(t)
	cfg, err := Load(lookup(values))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddress != ":8080" {
		t.Fatalf("shared HTTP default = %q", cfg.HTTPAddress)
	}
	if cfg.OperatorPublicURL != "http://localhost:8080" {
		t.Fatalf("public URL default = %q", cfg.OperatorPublicURL)
	}
	if cfg.OperatorTokenOrganization != "local" || cfg.OperatorTokenRole != "admin" {
		t.Fatalf("bootstrap scope = %q/%q", cfg.OperatorTokenOrganization, cfg.OperatorTokenRole)
	}
	if cfg.InvestigationWorkers != 8 || cfg.MaxPendingInvestigationsPerOrganization != 100 {
		t.Fatalf("investigation defaults = workers %d pending %d",
			cfg.InvestigationWorkers, cfg.MaxPendingInvestigationsPerOrganization)
	}
	if cfg.ModelContextWindowTokens != 128_000 {
		t.Fatalf("model context window default = %d", cfg.ModelContextWindowTokens)
	}
}

func TestLoadInvestigationLimitsFromEnvironment(t *testing.T) {
	values := essentialEnvironment(t)
	values[EnvInvestigationWorkers] = "3"
	values[EnvInvestigationMaxPendingPerOrganization] = "40"
	cfg, err := Load(lookup(values))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InvestigationWorkers != 3 || cfg.MaxPendingInvestigationsPerOrganization != 40 {
		t.Fatalf("investigation limits = workers %d pending %d",
			cfg.InvestigationWorkers, cfg.MaxPendingInvestigationsPerOrganization)
	}
}

func TestLoadModelContextWindowFromEnvironment(t *testing.T) {
	values := essentialEnvironment(t)
	values[EnvModelContextWindowSize] = "200000"
	cfg, err := Load(lookup(values))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ModelContextWindowTokens != 200_000 {
		t.Fatalf("model context window = %d", cfg.ModelContextWindowTokens)
	}
}

func TestLoadProcessUsesSmallYAMLSchemaAndEnvironmentPrecedence(t *testing.T) {
	dsn := secretFile(t, "postgres://user:password@localhost/opencluster")
	path := filepath.Join(t.TempDir(), "opencluster.yaml")
	document := "server:\n  address: ':9000'\n  public_url: http://localhost:9000\ndatabase:\n  dsn_file: " + dsn + "\n"
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadProcess(nil, lookup(map[string]string{
		EnvConfigFile: path, EnvHTTPAddress: ":9100",
		EnvOperatorTokenFile: secretFile(t, strings.Repeat("b", 32)),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddress != ":9100" || cfg.OperatorPublicURL != "http://localhost:9000" {
		t.Fatalf("resolved config = address %q public %q", cfg.HTTPAddress, cfg.OperatorPublicURL)
	}
}

func TestLoadProcessReadsInvestigationLimitsFromYAML(t *testing.T) {
	dsn := secretFile(t, "postgres://user:password@localhost/opencluster")
	path := filepath.Join(t.TempDir(), "opencluster.yaml")
	document := "database:\n  dsn_file: " + dsn +
		"\ninvestigation:\n  workers: 5\n  max_pending_per_organization: 75\n"
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadProcess(nil, lookup(map[string]string{
		EnvConfigFile: path, EnvOperatorTokenFile: secretFile(t, strings.Repeat("b", 32)),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InvestigationWorkers != 5 || cfg.MaxPendingInvestigationsPerOrganization != 75 {
		t.Fatalf("investigation limits = workers %d pending %d",
			cfg.InvestigationWorkers, cfg.MaxPendingInvestigationsPerOrganization)
	}
}

func TestLoadProcessReadsModelContextWindowFromYAML(t *testing.T) {
	dsn := secretFile(t, "postgres://user:password@localhost/opencluster")
	path := filepath.Join(t.TempDir(), "opencluster.yaml")
	document := "database:\n  dsn_file: " + dsn +
		"\nai:\n  context_window_tokens: 256000\n"
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadProcess(nil, lookup(map[string]string{
		EnvConfigFile: path, EnvOperatorTokenFile: secretFile(t, strings.Repeat("b", 32)),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ModelContextWindowTokens != 256_000 {
		t.Fatalf("model context window = %d", cfg.ModelContextWindowTokens)
	}
}

func TestLoadRejectsNonPositiveInvestigationLimits(t *testing.T) {
	for _, key := range []string{EnvInvestigationWorkers, EnvInvestigationMaxPendingPerOrganization} {
		t.Run(key, func(t *testing.T) {
			values := essentialEnvironment(t)
			values[key] = "0"
			if _, err := Load(lookup(values)); err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestLoadRejectsInvalidModelContextWindow(t *testing.T) {
	for _, value := range []string{"0", "-1", "not-a-number"} {
		t.Run(value, func(t *testing.T) {
			values := essentialEnvironment(t)
			values[EnvModelContextWindowSize] = value
			if _, err := Load(lookup(values)); err == nil ||
				!strings.Contains(err.Error(), EnvModelContextWindowSize) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestLoadProcessRejectsObsoleteYAMLSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencluster.yaml")
	if err := os.WriteFile(path, []byte("server:\n  operator_address: ':8080'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadProcess(nil, lookup(map[string]string{EnvConfigFile: path})); err == nil || !strings.Contains(err.Error(), "operator_address") {
		t.Fatalf("obsolete setting error = %v", err)
	}
}
