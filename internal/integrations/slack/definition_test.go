package slack

import (
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

func TestSlackInboundAvailabilityExplainsInstallationAndDeploymentSetup(t *testing.T) {
	t.Parallel()

	installed := integrations.Integration{Installation: &integrations.Installation{
		Key: integrations.InstallationKey{"A123", "T123"},
	}}
	tests := []struct {
		name          string
		servesEvents  bool
		integration   integrations.Integration
		wantAvailable bool
		wantReason    string
	}{
		{name: "pasted token", servesEvents: true,
			integration: integrations.Integration{Configuration: map[string]any{}},
			wantReason:  "installed Slack app"},
		{name: "no signed events endpoint", integration: installed,
			wantReason: "signing secret"},
		{name: "installed application", servesEvents: true, integration: installed,
			wantAvailable: true},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			availability := Definition(NewClient(""), nil, testCase.servesEvents).
				Inbound(testCase.integration)
			if availability.Available != testCase.wantAvailable {
				t.Errorf("inbound availability = %+v, want available %t",
					availability, testCase.wantAvailable)
			}
			if !strings.Contains(availability.Reason, testCase.wantReason) {
				t.Errorf("inbound reason = %q, want %q", availability.Reason, testCase.wantReason)
			}
		})
	}
}

func TestTheOnlyConfigurationFieldIsTheSecretToken(t *testing.T) {
	t.Parallel()

	definition := Definition(NewClient(""), nil, false)
	for _, field := range definition.Config {
		if field.Secret && field.Key != "botToken" {
			t.Errorf("%s is a second secret configuration field", field.Key)
		}
		if field.Required && field.Key != "botToken" {
			t.Errorf("%s is required, so the connect flow could not omit it", field.Key)
		}
	}
	token := definition.Config[0]
	if token.Key != "botToken" || !token.Secret || !token.Required {
		t.Errorf("botToken = %+v; it must be required and secret", token)
	}
	if token.Label != "Slack token" {
		t.Errorf("token label = %q, want Slack token", token.Label)
	}
	if !strings.Contains(string(definition.ConfigurationSchema()), `"writeOnly":true`) {
		t.Error("the rendered schema does not say the token is write-only")
	}
}

func TestEveryToolDeclaresItsWholeContract(t *testing.T) {
	t.Parallel()

	for _, tool := range Definition(NewClient(""), nil, false).Tools {
		if tool.Name == "" || !strings.HasPrefix(tool.Name, "slack_") {
			t.Errorf("tool name %q does not carry the provider prefix", tool.Name)
		}
		if strings.TrimSpace(tool.Description) == "" {
			t.Errorf("%s declares no description", tool.Name)
		}
		if tool.Run == nil {
			t.Errorf("%s declares no Run", tool.Name)
		}
		for _, argument := range tool.Arguments {
			if argument.Name == "" || argument.Description == "" || argument.Type == "" {
				t.Errorf("%s argument %+v is missing its name, description or type",
					tool.Name, argument)
			}
		}
	}
}

func TestEveryToolGrantIsKnownToVerification(t *testing.T) {
	t.Parallel()

	known := map[string]string{}
	for scope, cost := range requiredScopes {
		known[scope] = cost
	}
	for scope, cost := range optionalScopes {
		known[scope] = cost
	}

	for _, tool := range Definition(NewClient(""), nil, false).Tools {
		for _, grant := range tool.RequiredGrants {
			if grant == grantUserToken {
				continue
			}
			if _, exists := known[grant]; !exists {
				t.Errorf("%s requires unknown grant %s", tool.Name, grant)
			}
		}
	}
}

func TestMessageDescriptionsRequireCorroboration(t *testing.T) {
	t.Parallel()

	for _, tool := range Definition(NewClient(""), nil, false).Tools {
		if tool.Name == "slack_list_channels" {
			continue
		}
		const limitation = "testimony and requires corroboration"
		if !strings.Contains(strings.ToLower(tool.Description), limitation) {
			t.Errorf("%s description does not say %q: %q", tool.Name, limitation, tool.Description)
		}
	}
}
