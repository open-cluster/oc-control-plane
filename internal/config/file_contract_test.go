package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestConfigurationDocumentationMatchesYAMLSchema(t *testing.T) {
	t.Parallel()

	path := filepath.Join("..", "..", "docs", "self-hosted", "configuration.mdx")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	fences := regexp.MustCompile("(?s)```yaml\\r?\\n(.*?)```").FindAllSubmatch(content, -1)
	if len(fences) == 0 {
		t.Fatal("configuration documentation contains no YAML examples")
	}
	var examples strings.Builder
	for index, fence := range fences {
		decoder := yaml.NewDecoder(bytes.NewReader(fence[1]))
		decoder.KnownFields(true)
		var document fileDocument
		if err := decoder.Decode(&document); err != nil {
			t.Errorf("YAML example %d does not match the production schema: %v", index+1, err)
		}
		examples.Write(fence[1])
	}

	for _, name := range yamlLeafNames(reflect.TypeOf(fileDocument{})) {
		pattern := regexp.MustCompile("(?m)^\\s*" + regexp.QuoteMeta(name) + ":")
		if !pattern.MatchString(examples.String()) {
			t.Errorf("production YAML setting %q is absent from the documented examples", name)
		}
	}

	for _, reference := range []string{
		"dsn_file", "bootstrap_token_file", "sealing_key_file", "client_secret_file",
		"signing_secret_file", "app_private_key_file", "api_key_file",
	} {
		if !regexp.MustCompile("(?m)^\\s*" + reference + ":").MatchString(examples.String()) {
			t.Errorf("secret reference %q is absent from the documented examples", reference)
		}
	}
	if !bytes.Contains(content, []byte("Settings ending in `_file` name a")) {
		t.Error("documentation must state that secret settings name mounted files")
	}
	for _, contract := range []string{
		EnvDatabaseDSNFile,
		"split them into one control-plane deployment",
		"restart the prior binary",
		"30 seconds to read a request",
	} {
		if !bytes.Contains(content, []byte(contract)) {
			t.Errorf("Phase 2 migration contract %q is absent from the configuration reference",
				contract)
		}
	}

	for _, documentedDefault := range []string{
		defaultShutdownTimeout.String(),
		defaultServiceName,
		fmt.Sprint(defaultChangeLedgerRetentionDays),
		fmt.Sprint(defaultModelSpendCeilingCents),
		fmt.Sprint(defaultOrgConcurrentInvestigations),
	} {
		quoted := "`" + documentedDefault + "`"
		if !bytes.Contains(content, []byte(quoted)) {
			t.Errorf("built-in default %s is absent from the configuration reference", quoted)
		}
	}
	for documented, configured := range map[string]string{
		"5m": defaultInventoryInterval.String(),
		"2h": defaultInvestigationWindowLead.String(),
	} {
		if !bytes.Contains(content, []byte("Default: `"+documented+"`")) {
			t.Errorf("built-in duration %s (%s) is absent from the configuration reference",
				documented, configured)
		}
	}
}

func TestConversationInternalsAreNotDeploymentConfiguration(t *testing.T) {
	t.Parallel()

	for _, setting := range yamlLeafNames(reflect.TypeOf(fileDocument{})) {
		for _, retired := range []string{
			"context_window", "context_threshold_percent", "max_waiting_investigations", "enabled",
		} {
			if setting == retired {
				t.Errorf("platform-owned conversation setting %q must not appear in the YAML schema", retired)
			}
		}
	}
	content, err := os.ReadFile(filepath.Join("..", "..", "docs", "self-hosted", "configuration.mdx"))
	if err != nil {
		t.Fatal(err)
	}
	for _, retired := range []string{
		"OC_CONVERSATIONS_ENABLED", "OC_ORG_MAX_WAITING_INVESTIGATIONS",
		"OC_MODEL_CONTEXT_WINDOW", "OC_CONTEXT_THRESHOLD_PERCENT",
	} {
		if bytes.Contains(content, []byte(retired)) {
			t.Errorf("platform-owned conversation setting %q must not be documented", retired)
		}
	}
}

func yamlLeafNames(value reflect.Type) []string {
	var names []string
	for index := 0; index < value.NumField(); index++ {
		field := value.Field(index)
		name := strings.Split(field.Tag.Get("yaml"), ",")[0]
		fieldType := field.Type
		for fieldType.Kind() == reflect.Pointer || fieldType.Kind() == reflect.Map ||
			fieldType.Kind() == reflect.Slice {
			fieldType = fieldType.Elem()
		}
		if fieldType.Kind() == reflect.Struct {
			names = append(names, yamlLeafNames(fieldType)...)
			continue
		}
		names = append(names, name)
	}
	return names
}
