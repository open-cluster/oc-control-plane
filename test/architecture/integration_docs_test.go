package gates_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/integrations/alertmanager"
	"github.com/open-cluster/oc-control-plane/internal/integrations/genericwebhook"
	"github.com/open-cluster/oc-control-plane/internal/integrations/github"
	"github.com/open-cluster/oc-control-plane/internal/integrations/kubernetes"
	"github.com/open-cluster/oc-control-plane/internal/integrations/slack"
)

func TestEveryIntegrationHasItsDocumentedProductRolePage(t *testing.T) {
	t.Parallel()

	catalog, err := integrations.NewCatalog(
		alertmanager.Definition(),
		kubernetes.Definition(),
		slack.Definition(slack.NewClient(""), nil, false),
		github.Definition(nil, github.NewClient("")),
		genericwebhook.Definition(),
	)
	if err != nil {
		t.Fatal(err)
	}
	navigation, err := os.ReadFile(filepath.Join(moduleRoot, "docs", "docs.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, manifest := range catalog.Manifests() {
		slug := "integrations/" + string(manifest.Category) + "/" + manifest.Key
		if manifest.DocumentationSlug != slug {
			t.Errorf("Integration %q documentation slug = %q, want %q", manifest.Key, manifest.DocumentationSlug, slug)
		}
		if _, err := os.Stat(filepath.Join(moduleRoot, "docs", filepath.FromSlash(slug)+".mdx")); err != nil {
			t.Errorf("Integration %q has no documentation page: %v", manifest.Key, err)
		}
		if !strings.Contains(string(navigation), `"`+slug+`"`) {
			t.Errorf("Integration %q documentation is absent from navigation", manifest.Key)
		}
	}
}
