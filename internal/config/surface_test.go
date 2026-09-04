package config

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestSupportedEnvironmentSurfaceIsSmallAndProductFacing(t *testing.T) {
	t.Parallel()
	want := []string{
		"OC_CONFIG_FILE",
		"OC_SERVER_ADDRESS", "OC_PUBLIC_URL",
		"OC_DATABASE_DSN_FILE",
		"OC_AUTH_MODE", "OC_BOOTSTRAP_TOKEN_FILE",
		"OC_OIDC_ISSUER", "OC_OIDC_CLIENT_ID", "OC_OIDC_CLIENT_SECRET_FILE",
		"OC_RELAY_ADDRESS", "OC_RELAY_SPKI_PINS",
		"OC_AI_PROVIDER", "OC_AI_MODEL", "OC_AI_API_KEY_FILE", "OC_AI_CONTEXT_WINDOW_SIZE",
		"OC_INVESTIGATION_WORKERS", "OC_MAX_PENDING_INVESTIGATIONS_PER_ORGANIZATION",
		"OC_ENCRYPTION_KEY_FILE",
		"OC_LOG_LEVEL", "OC_OTLP_ENDPOINT",
		"OC_SLACK_CLIENT_ID", "OC_SLACK_CLIENT_SECRET_FILE", "OC_SLACK_SIGNING_SECRET_FILE",
		"OC_GITHUB_APP_ID", "OC_GITHUB_APP_PRIVATE_KEY_FILE",
	}
	if !reflect.DeepEqual(SupportedEnvironmentKeys, want) {
		t.Fatalf("supported environment keys = %#v, want %#v", SupportedEnvironmentKeys, want)
	}
	if len(SupportedEnvironmentKeys) > 25 {
		t.Fatalf("supported environment surface has %d keys, maximum is 25", len(SupportedEnvironmentKeys))
	}
}

func TestRetiredConfigurationImplementationIsAbsent(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"config.go", "operator.go"} {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, retired := range []string{
			"OC_PLACEMENTS", "OC_OPERATOR_ADDRESS", "OC_INTAKE_ADDRESS",
			"OC_HOSTED_MODE", "OC_WORKOS_", "OC_LEGACY_IDENTITY_",
			"OC_PREVIOUS_SEALING_KEY_FILES", "OC_MODEL_EFFORT",
			"OC_MODEL_SPEND_CEILING_CENTS", "OC_INVESTIGATION_MAX_",
			"OC_GITHUB_APP_SLUG", "OC_SLACK_AGENT_ORGANIZATIONS",
		} {
			if strings.Contains(string(raw), retired) {
				t.Errorf("%s retains retired configuration %q", name, retired)
			}
		}
	}
}
