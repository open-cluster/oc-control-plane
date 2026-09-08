package config

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestLoadAcceptsDirectSecretsWithoutChangingConfiguration(t *testing.T) {
	files := essentialEnvironment(t)
	for key, value := range map[string]string{
		EnvAuthenticationMode: "local+oidc", EnvOIDCIssuer: "https://identity.example.com",
		EnvOIDCClientID: "client", EnvSlackClientID: "slack-client", EnvGitHubAppID: "123",
	} {
		files[key] = value
	}
	secrets := map[string]string{
		EnvOIDCClientSecret: "oidc-secret", EnvSlackClientSecret: "slack-secret",
		EnvSlackSigningSecret: "signing-secret", EnvGitHubAppKey: "private-key",
	}
	for key, value := range secrets {
		files[key+"_FILE"] = secretFile(t, value)
	}
	expected, err := Load(lookup(files))
	if err != nil {
		t.Fatal(err)
	}
	direct := map[string]string{
		"OC_DATABASE_DSN":    "postgres://user:password@localhost/opencluster",
		"OC_BOOTSTRAP_TOKEN": strings.Repeat("b", 32),
		"OC_ENCRYPTION_KEY":  strings.Repeat("k", 32),
		EnvModelProvider:     "anthropic", EnvModelName: "model", "OC_AI_API_KEY": "model-key",
	}
	for _, key := range []string{EnvAuthenticationMode, EnvOIDCIssuer, EnvOIDCClientID, EnvSlackClientID, EnvGitHubAppID} {
		direct[key] = files[key]
	}
	for key, value := range secrets {
		direct[key] = value
	}
	actual, err := Load(lookup(direct))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatal("direct and file inputs produced different configuration")
	}
	for _, key := range []string{EnvDatabaseDSN, EnvOperatorToken, EnvSealingKey, EnvModelKey,
		EnvOIDCClientSecret, EnvSlackClientSecret, EnvSlackSigningSecret, EnvGitHubAppKey} {
		conflicting := make(map[string]string, len(direct)+1)
		for name, value := range direct {
			conflicting[name] = value
		}
		conflicting[key+"_FILE"] = "private-path-do-not-disclose"
		_, err := Load(lookup(conflicting))
		if err == nil || !strings.Contains(err.Error(), key) || strings.Contains(err.Error(), "private-path") || strings.Contains(err.Error(), direct[key]) {
			t.Fatalf("%s conflict was not safely rejected: %v", key, err)
		}
	}
}

func TestSecretFilesAreReadAtStartupAndFailuresAreRedacted(t *testing.T) {
	values := essentialEnvironment(t)
	cfg, err := Load(lookup(values))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(values[EnvModelKeyFile], []byte("rotated-model-key"), 0600); err != nil {
		t.Fatal(err)
	}
	if cfg.ModelKey != "model-key" {
		t.Fatal("file change altered loaded configuration")
	}
	restarted, err := Load(lookup(values))
	if err != nil || restarted.ModelKey != "rotated-model-key" {
		t.Fatalf("restart did not read rotated key: %v", err)
	}
	for _, invalid := range []map[string]string{
		{EnvModelKeyFile: "private-missing-path"},
		{EnvModelKeyFile: secretFile(t, " ")},
		{EnvSealingKeyFile: "", EnvSealingKey: "private-invalid-key"},
		{EnvLogLevel: "private-invalid-level"},
	} {
		candidate := make(map[string]string, len(values)+1)
		for key, value := range values {
			candidate[key] = value
		}
		for key, value := range invalid {
			candidate[key] = value
		}
		_, err := Load(lookup(candidate))
		if err == nil || strings.Contains(err.Error(), "private-") {
			t.Fatalf("invalid input was not safely refused: %v", err)
		}
	}
}
