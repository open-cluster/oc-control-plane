package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
		EnvDatabaseDSNFile:    secretFile(t, "postgres://user:password@localhost/opencluster"),
		EnvBootstrapTokenFile: secretFile(t, strings.Repeat("b", 32)),
		EnvSealingKeyFile:     secretFile(t, strings.Repeat("k", 32)),
		EnvModelProvider:      "anthropic", EnvModelName: "model",
		EnvModelKeyFile: secretFile(t, "model-key"),
	}
}

func TestLoadWithoutBootstrapCredential(t *testing.T) {
	values := essentialEnvironment(t)
	delete(values, EnvBootstrapTokenFile)
	cfg, err := Load(lookup(values))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.BootstrapTokenDigest) != 0 {
		t.Fatal("bootstrap was enabled without a credential")
	}
}

func TestMissingSecretsNameDirectAndFileInputs(t *testing.T) {
	t.Run("database", func(t *testing.T) {
		values := essentialEnvironment(t)
		delete(values, EnvDatabaseDSNFile)
		_, err := Load(lookup(values))
		if err == nil || !strings.Contains(err.Error(), EnvDatabaseDSN+" or "+EnvDatabaseDSNFile) {
			t.Fatalf("missing database secret = %v", err)
		}
	})

	t.Run("model", func(t *testing.T) {
		values := essentialEnvironment(t)
		delete(values, EnvModelKeyFile)
		_, err := Load(lookup(values))
		if err == nil || !strings.Contains(err.Error(), EnvModelKey+" or "+EnvModelKeyFile) {
			t.Fatalf("missing model secret = %v", err)
		}
	})
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
	if cfg.HTTPListenAddress != ":8081" {
		t.Fatalf("shared HTTP default = %q", cfg.HTTPListenAddress)
	}
	if cfg.PublicURL != "http://localhost:8081" {
		t.Fatalf("public URL default = %q", cfg.PublicURL)
	}
	if cfg.InvestigationWorkers != 8 || cfg.MaxPendingInvestigations != 100 {
		t.Fatalf("investigation defaults = workers %d pending %d",
			cfg.InvestigationWorkers, cfg.MaxPendingInvestigations)
	}
	if cfg.SessionLifetime != 12*time.Hour {
		t.Fatalf("session lifetime default = %v", cfg.SessionLifetime)
	}
	if cfg.ModelContextWindow != 0 || cfg.ModelMaxOutputTokens != 0 {
		t.Fatalf("model limit overrides = context %d output %d",
			cfg.ModelContextWindow, cfg.ModelMaxOutputTokens)
	}
}

func TestLoadPublicBrowserOrigins(t *testing.T) {
	for _, test := range []struct{ input, normalized string }{
		{"https://console.example.com", "https://console.example.com"},
		{" HTTPS://Console.Example.COM:443/ ", "https://console.example.com:443"},
		{"https://192.0.2.1:8443", "https://192.0.2.1:8443"},
		{"https://[2001:db8::1]", "https://[2001:db8::1]"},
		{"https://[2001:0db8:0:0:0:0:0:1]", "https://[2001:db8::1]"},
		{"https://[::ffff:192.0.2.1]", ""},
		{"http://localhost:8081/", "http://localhost:8081"},
		{"http://127.0.0.1:8081", "http://127.0.0.1:8081"},
		{"http://[::1]:8081", "http://[::1]:8081"},
		{"ftp://localhost", ""}, {"http://console.example.com", ""},
		{"http://localhost.example.com", ""}, {"http://127.0.0.2", ""},
		{"https://user:private@console.example.com", ""},
		{"https://console.example.com/path", ""}, {"https://console.example.com//", ""},
		{"https://console.example.com/%2f", ""},
		{"https://console.example.com?private=value", ""}, {"https://console.example.com?", ""},
		{"https://console.example.com#private", ""}, {"https://console.example.com#", ""},
		{"https://:8081", ""},
		{"https://999.1.2.3", ""},
		{"https://0x7f000001", ""},
		{"https://[localhost]", ""}, {"https://::1", ""},
		{"https://console.example.com:", ""}, {"https://console.example.com:zero", ""},
		{"https://console.example.com:0", ""}, {"https://console.example.com:65536", ""},
	} {
		t.Run(test.input, func(t *testing.T) {
			values := essentialEnvironment(t)
			values[EnvPublicURL] = test.input
			cfg, err := Load(lookup(values))
			if test.normalized == "" {
				if err == nil || !strings.Contains(err.Error(), EnvPublicURL) || strings.Contains(err.Error(), "private") {
					t.Fatalf("origin was accepted or its error was unsafe: %v", err)
				}
			} else if err != nil || cfg.PublicURL != test.normalized {
				t.Fatalf("origin=%q error=%v, want %q", cfg.PublicURL, err, test.normalized)
			}
		})
	}
}

func TestLoadEnvironmentOverridesYAMLAndKeepsPublicOriginIndependent(t *testing.T) {
	values := essentialEnvironment(t)
	values[EnvConfigFile] = secretFile(t, "server:\n  address: '127.0.0.1:9091'\n  publicURL: 'https://console.example.com'\nai:\n  apiKeyFile: 'missing-key'\n")
	delete(values, EnvModelKeyFile)
	values[EnvModelKey] = "environment-key"
	cfg, err := Load(lookup(values))
	if err != nil || cfg.HTTPListenAddress != "127.0.0.1:9091" || cfg.PublicURL != "https://console.example.com" || cfg.ModelAPIKey != "environment-key" {
		t.Fatalf("YAML fallback or direct-secret override failed: %v", err)
	}
	values[EnvHTTPAddress] = "127.0.0.1:9092"
	cfg, err = Load(lookup(values))
	if err != nil || cfg.HTTPListenAddress != "127.0.0.1:9092" || cfg.PublicURL != "https://console.example.com" {
		t.Fatalf("listener override changed the public origin: %v", err)
	}
}

func TestLoadSessionLifetimeFromEnvironment(t *testing.T) {
	values := essentialEnvironment(t)
	values[EnvSessionLifetimeSeconds] = "900"
	cfg, err := Load(lookup(values))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SessionLifetime != 15*time.Minute {
		t.Fatalf("session lifetime = %v", cfg.SessionLifetime)
	}
}

func TestLoadRejectsInvalidSessionLifetime(t *testing.T) {
	for _, value := range []string{"0", "-1", "299", "2592001"} {
		t.Run(value, func(t *testing.T) {
			values := essentialEnvironment(t)
			values[EnvSessionLifetimeSeconds] = value
			if _, err := Load(lookup(values)); err == nil || !strings.Contains(err.Error(), EnvSessionLifetimeSeconds) {
				t.Fatalf("loading lifetime %q returned %v", value, err)
			}
		})
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
	if cfg.InvestigationWorkers != 3 || cfg.MaxPendingInvestigations != 40 {
		t.Fatalf("investigation limits = workers %d pending %d",
			cfg.InvestigationWorkers, cfg.MaxPendingInvestigations)
	}
}

func TestLoadModelContextWindowFromEnvironment(t *testing.T) {
	values := essentialEnvironment(t)
	values[EnvModelContextWindowSize] = "200000"
	cfg, err := Load(lookup(values))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ModelContextWindow != 200_000 {
		t.Fatalf("model context window = %d", cfg.ModelContextWindow)
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

func TestLoadRejectsPartialOIDCConfiguration(t *testing.T) {
	values := essentialEnvironment(t)
	values[EnvOIDCIssuer] = "https://identity.example.com"
	if _, err := Load(lookup(values)); err == nil || !strings.Contains(err.Error(), EnvOIDCClientID) {
		t.Fatalf("partial OIDC configuration = %v", err)
	}
}
