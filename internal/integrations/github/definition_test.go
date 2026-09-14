package github

import (
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

func TestDefinitionDeclaresProviderContract(t *testing.T) {
	t.Parallel()

	definition := Definition(nil, NewClient(""))
	if definition.Type != integrations.TypeGitHub || definition.Key != "github" {
		t.Errorf("identity = %d %q", definition.Type, definition.Key)
	}
	if definition.Category != integrations.CategorySourceControl {
		t.Errorf("category = %q", definition.Category)
	}
	if definition.RequiresRelay {
		t.Error("github needs no relay")
	}
	if definition.Verify != nil || definition.Probe == nil {
		t.Error("github verifies by probing the live installation, not from gathered facts")
	}
	const wantDescription = "Give investigations read-only access to selected repositories " +
		"for commits, pull requests, CI failures, files, and releases."
	if definition.Description != wantDescription {
		t.Errorf("description = %q, want %q", definition.Description, wantDescription)
	}
}

func TestConfiguredAppProvidesConnectionFlow(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	fake.answer("/app", `{"slug":"opencluster"}`)
	fake.answer("/app/installations/77", `{
		"account":{"login":"acme-corp","type":"Organization"},
		"repository_selection":"selected","suspended_at":null}`)
	app, err := NewApp("12345", pemPKCS1(testKey(t)), NewClient(fake.URL))
	if err != nil {
		t.Fatalf("building app: %v", err)
	}

	connected := Definition(app, NewClient(fake.URL)).Connect
	if connected == nil {
		t.Fatal("configured GitHub app has no connection flow")
	}
	location, err := connected.Authorize(testContext(t), "opaque-state", "https://control.example/callback")
	if err != nil || !strings.Contains(location, "/apps/opencluster/installations/new?state=opaque-state") {
		t.Fatalf("authorization location = %q, error = %v", location, err)
	}
	bound, err := connected.Redeem(testContext(t), integrations.ConnectReturn{
		Query: map[string][]string{"installation_id": {"77"}},
	})
	if err != nil {
		t.Fatalf("redeeming installation: %v", err)
	}
	if bound.Name != "GitHub — acme-corp" || len(bound.Configuration) != 0 {
		t.Errorf("binding = %+v", bound)
	}
	if bound.Installation == nil || bound.Installation.Application != "github" ||
		bound.Installation.Workspace != "77" {
		t.Errorf("installation = %+v", bound.Installation)
	}
	if fake.called("/app") != 1 || fake.called("/app/installations/77") != 1 {
		t.Errorf("provider calls: app=%d installation=%d", fake.called("/app"),
			fake.called("/app/installations/77"))
	}
}

func TestInstallationIdentityIsNotEditableConfiguration(t *testing.T) {
	t.Parallel()

	definition := Definition(nil, NewClient(""))
	if len(definition.Config) != 0 {
		t.Fatalf("config declares discovered fields: %+v", definition.Config)
	}
}

func TestEveryToolDeclaresItsWholeContract(t *testing.T) {
	t.Parallel()

	for _, tool := range Definition(nil, NewClient("")).Tools {
		if !strings.HasPrefix(tool.Name, "github.") {
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

func TestInstallationOfRefusesWhatIsNotAnID(t *testing.T) {
	t.Parallel()

	for name, installed := range map[string]*integrations.Installation{
		"absent":   nil,
		"text":     {Workspace: "abc"},
		"negative": {Workspace: "-1"},
	} {
		integration := integrations.Integration{Installation: installed}
		if _, err := installationOf(integration); err == nil {
			t.Errorf("an %s installation id was accepted", name)
		}
	}

	integration := integrations.Integration{
		Installation: &integrations.Installation{Workspace: "77"},
	}
	installation, err := installationOf(integration)
	if err != nil || installation != 77 {
		t.Errorf("a whole id was refused: %v %d", err, installation)
	}
}
