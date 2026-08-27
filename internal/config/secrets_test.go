package config_test

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/config"
)

func lookupFrom(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func TestMountedSecretSourceReadsAFileWithoutExposingItsContents(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	path := filepath.Join(directory, "model-key")
	if err := os.WriteFile(path, []byte("mounted-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := config.MountedSecretSource{}
	value, err := source.Read("OC_MODEL_KEY_FILE", path)
	if err != nil || strings.TrimSpace(string(value)) != "mounted-value" {
		t.Fatalf("read mounted secret = %q, %v", string(value), err)
	}

	missing := filepath.Join(directory, "value-is-not-an-error-message")
	_, err = source.Read("OC_MODEL_KEY_FILE", missing)
	if err == nil || !strings.Contains(err.Error(), "OC_MODEL_KEY_FILE") ||
		strings.Contains(err.Error(), "value-is-not-an-error-message") {
		t.Fatalf("missing-file error exposed the reference or omitted the setting: %v", err)
	}
}

func TestEnvironmentSecretSourceIsExplicitlyDevelopmentOnlyAndWarnsSafely(t *testing.T) {
	t.Parallel()

	lookup := lookupFrom(map[string]string{"LOCAL_MODEL_KEY": "development-value"})
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, nil))

	production := config.EnvironmentSecretSource{Lookup: lookup, Logger: logger}
	if _, err := production.Read("model credential", "LOCAL_MODEL_KEY"); err == nil {
		t.Fatal("an environment secret was accepted outside development mode")
	}
	if output.Len() != 0 {
		t.Fatalf("a refused source logged unexpectedly: %s", output.String())
	}

	development := config.EnvironmentSecretSource{
		Development: true,
		Lookup:      lookup,
		Logger:      logger,
	}
	value, err := development.Read("model credential", "LOCAL_MODEL_KEY")
	if err != nil || string(value) != "development-value" {
		t.Fatalf("read development secret = %q, %v", string(value), err)
	}
	logged := output.String()
	if !strings.Contains(logged, "development-only") ||
		!strings.Contains(logged, "model credential") ||
		strings.Contains(logged, "development-value") {
		t.Fatalf("development warning was missing or exposed the secret: %s", logged)
	}
}
