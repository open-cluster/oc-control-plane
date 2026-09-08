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

func TestLoadWithoutBootstrapCredential(t *testing.T) {
	values := essentialEnvironment(t)
	delete(values, EnvOperatorTokenFile)
	cfg, err := Load(lookup(values))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.OperatorTokenDigest) != 0 {
		t.Fatal("bootstrap was enabled without a credential")
	}
}

func TestRecoveryNeedsOnlyDeploymentDatabaseConfiguration(t *testing.T) {
	values := map[string]string{EnvDatabaseDSNFile: secretFile(t, "postgres://user:password@localhost/opencluster"), EnvModelProvider: "anthropic"}
	dsn, err := LoadRecoveryDatabase(lookup(values))
	if err != nil || dsn != "postgres://user:password@localhost/opencluster" {
		t.Fatalf("recovery database configuration failed: %v", err)
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
	if cfg.InvestigationWorkers != 8 || cfg.MaxPendingInvestigationsPerOrganization != 100 {
		t.Fatalf("investigation defaults = workers %d pending %d",
			cfg.InvestigationWorkers, cfg.MaxPendingInvestigationsPerOrganization)
	}
	if cfg.ModelContextWindowTokens != 0 || cfg.ModelMaxOutputTokens != 0 {
		t.Fatalf("model limit overrides = context %d output %d",
			cfg.ModelContextWindowTokens, cfg.ModelMaxOutputTokens)
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

func TestLoadModelOutputLimitFromEnvironment(t *testing.T) {
	values := essentialEnvironment(t)
	values[EnvModelMaxOutputTokens] = "64000"
	cfg, err := Load(lookup(values))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ModelMaxOutputTokens != 64_000 {
		t.Fatalf("model output limit = %d", cfg.ModelMaxOutputTokens)
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

func TestLoadRejectsInvalidModelLimits(t *testing.T) {
	for _, key := range []string{EnvModelContextWindowSize, EnvModelMaxOutputTokens} {
		for _, value := range []string{"0", "-1", "not-a-number"} {
			t.Run(key+"/"+value, func(t *testing.T) {
				values := essentialEnvironment(t)
				values[key] = value
				if _, err := Load(lookup(values)); err == nil || !strings.Contains(err.Error(), key) {
					t.Fatalf("error = %v", err)
				}
			})
		}
	}
}

func TestLoadProcessAcceptsEnvironmentAndRejectsRetiredConfiguration(t *testing.T) {
	values := essentialEnvironment(t)
	cfg, err := LoadProcess([]string{"--server-address", ":9100"}, lookup(values))
	if err != nil || cfg.HTTPAddress != ":9100" {
		t.Fatalf("startup configuration: %v", err)
	}
	if _, err := LoadProcess([]string{"--config", "private-path"}, lookup(values)); err == nil || strings.Contains(err.Error(), "private-path") {
		t.Fatalf("retired flag: %v", err)
	}
	values["OC_CONFIG_FILE"] = "private-path"
	if _, err := LoadProcess(nil, lookup(values)); err == nil || strings.Contains(err.Error(), "private-path") {
		t.Fatalf("retired setting: %v", err)
	}
}
