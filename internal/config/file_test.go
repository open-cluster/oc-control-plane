package config

import (
	"bytes"
	"encoding/base64"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadAppliesDefaultsThenYAMLThenEnvironment(t *testing.T) {
	directory := t.TempDir()
	write := func(name, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(directory, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("database", "postgres://user:password@localhost/opencluster")
	write("model", "model-key")
	write("encryption", strings.Repeat("k", 32))
	write("bootstrap", strings.Repeat("b", 32))
	write("oidc", "oidc-secret")
	write("slack-client", "slack-client-secret")
	write("slack-signing", "slack-signing-secret")
	write("github", "github-private-key")
	pin := base64.StdEncoding.EncodeToString(make([]byte, 32))
	overridePin := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))

	configPath := filepath.Join(directory, "config.yaml")
	yaml := `server:
  address: ":8181"
  publicURL: "https://file.example"
telemetry:
  logLevel: warn
  otlpEndpoint: "collector.example:4317"
database:
  dsnFile: database
session:
  lifetime: "24h"
relay:
  address: ":8444"
  spkiPins: ["` + pin + `"]
ai:
  provider: anthropic
  model: file-model
  apiKeyFile: model
  contextWindow: 200000
  maxOutputTokens: 32000
investigations:
  workers: 4
  maxPendingPerOrganization: 40
oidc:
  issuer: "https://identity.example"
  clientID: opencluster
  clientSecretFile: oidc
slack:
  clientID: slack-client
  clientSecretFile: slack-client
  signingSecretFile: slack-signing
github:
  appID: "12345"
  privateKeyFile: github
security:
  encryptionKeyFile: encryption
  bootstrapTokenFile: bootstrap
`
	if err := os.WriteFile(configPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(lookup(map[string]string{
		EnvConfigFile:    configPath,
		EnvHTTPAddress:   ":9191",
		EnvRelaySPKIPins: overridePin,
		EnvModelKey:      "environment-model-key",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPListenAddress != ":9191" || cfg.PublicURL != "https://file.example" {
		t.Fatalf("server = %q, %q", cfg.HTTPListenAddress, cfg.PublicURL)
	}
	if cfg.LogLevel != slog.LevelWarn || cfg.OTLPEndpoint != "collector.example:4317" ||
		cfg.RelayListenAddress != ":8444" {
		t.Fatalf("telemetry/relay = %s, %q, %q", cfg.LogLevel, cfg.OTLPEndpoint, cfg.RelayListenAddress)
	}
	if cfg.DatabaseDSN == "" || cfg.ModelAPIKey != "environment-model-key" || len(cfg.SealingKey) != 32 ||
		len(cfg.BootstrapTokenDigest) != 32 || cfg.OIDCClientSecret != "oidc-secret" {
		t.Fatal("relative secret files were not loaded from the configuration directory")
	}
	if cfg.OIDCIssuer != "https://identity.example" || cfg.OIDCClientID != "opencluster" ||
		cfg.SlackClientID != "slack-client" || cfg.SlackClientSecret != "slack-client-secret" ||
		cfg.SlackSigningSecret != "slack-signing-secret" || cfg.GitHubAppID != "12345" ||
		string(cfg.GitHubAppPrivateKey) != "github-private-key" ||
		cfg.ModelProvider != "anthropic" || cfg.ModelName != "file-model" {
		t.Fatalf("integration configuration was not loaded: %+v", cfg)
	}
	if cfg.SessionLifetime != 24*time.Hour || cfg.InvestigationWorkers != 4 ||
		cfg.MaxPendingInvestigations != 40 || cfg.ModelContextWindow != 200000 ||
		cfg.ModelMaxOutputTokens != 32000 {
		t.Fatalf("file values were not applied: %+v", cfg)
	}
	if len(cfg.RelaySPKIPins) != 1 || cfg.RelaySPKIPins[0] != overridePin {
		t.Fatalf("environment did not replace YAML relay pins: %v", cfg.RelaySPKIPins)
	}
}

func TestLoadRejectsUnknownOrTrailingYAML(t *testing.T) {
	for name, yaml := range map[string]string{
		"unknown":          "server:\n  adress: ':8080'\n",
		"duplicate":        "server:\n  address: ':8080'\n  address: ':9090'\n",
		"plaintext secret": "ai:\n  apiKey: do-not-accept\n",
		"invalid duration": "session:\n  lifetime: tomorrow\n",
		"trailing":         "server:\n  address: ':8080'\n---\nserver:\n  address: ':9090'\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(lookup(map[string]string{EnvConfigFile: path}))
			if err == nil || strings.Contains(err.Error(), path) {
				t.Fatalf("invalid YAML was not safely rejected: %v", err)
			}
		})
	}

	missing := filepath.Join(t.TempDir(), "missing.yaml")
	if _, err := Load(lookup(map[string]string{EnvConfigFile: missing})); err == nil || strings.Contains(err.Error(), missing) {
		t.Fatalf("unreadable configuration was not safely rejected: %v", err)
	}
}
