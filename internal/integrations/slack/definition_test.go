package slack

import (
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

func TestDefinitionDeclaresProviderContract(t *testing.T) {
	t.Parallel()

	definition := Definition(NewClient(""), nil, false)
	if definition.Key != "slack" {
		t.Errorf("identity = %q", definition.Key)
	}
	if definition.Category != integrations.CategoryCollaboration {
		t.Errorf("category = %q", definition.Category)
	}
	if definition.RequiresRelay {
		t.Error("slack needs no relay")
	}
	if definition.Verify != nil {
		t.Error("a credential-bearing type verifies by probing live, not from gathered facts")
	}
	if definition.Probe == nil {
		t.Error("no probe; nothing could ever verify a pasted token against the vendor")
	}
	const wantDescription = "Give investigations read-only access to Slack conversations visible " +
		"to the connected token and reply to direct app mentions in their original thread."
	if definition.Description != wantDescription {
		t.Errorf("description = %q, want %q", definition.Description, wantDescription)
	}
}

func TestSlackInboundAvailabilityExplainsInstallationAndDeploymentSetup(t *testing.T) {
	t.Parallel()

	installed := integrations.Integration{Installation: &integrations.Installation{
		Application: "A123", Workspace: "T123",
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

// Every tool carries its full contract. The model chooses by this metadata, so
// an empty field is not missing documentation — it is a tool that will be misrouted and
// then patched with prompts.
func TestEveryToolDeclaresItsWholeContract(t *testing.T) {
	t.Parallel()

	for _, tool := range Definition(NewClient(""), nil, false).Tools {
		if tool.Name == "" || !strings.HasPrefix(tool.Name, "slack.") {
			t.Errorf("tool name %q does not carry the provider prefix", tool.Name)
		}
		for field, value := range map[string]string{
			"description":  tool.Description,
			"whenToUse":    tool.WhenToUse,
			"whenNotToUse": tool.WhenNotToUse,
			"permissions":  tool.Permissions,
			"output":       tool.Output,
		} {
			if strings.TrimSpace(value) == "" {
				t.Errorf("%s declares no %s", tool.Name, field)
			}
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

// The scopes verification checks are exactly the ones the tools claim to need, so a scope
// can be neither demanded for nothing nor needed silently.
func TestVerifiedScopesMatchWhatTheToolsClaim(t *testing.T) {
	t.Parallel()

	// Both halves: a required scope must be needed by something, and an optional one must
	// be worth holding. A scope in neither map is a scope nothing checks.
	known := map[string]string{}
	for scope, cost := range requiredScopes {
		known[scope] = cost
	}
	for scope, cost := range optionalScopes {
		known[scope] = cost
	}

	claimed := map[string]bool{}
	for _, tool := range Definition(NewClient(""), nil, false).Tools {
		for scope := range known {
			if strings.Contains(tool.Permissions, scope) {
				claimed[scope] = true
			}
		}
	}
	for scope := range known {
		if !claimed[scope] {
			t.Errorf("verification knows %s and no tool claims to need it", scope)
		}
	}
}
