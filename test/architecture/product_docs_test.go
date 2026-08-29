package gates_test

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/integrations/alertmanager"
	"github.com/open-cluster/oc-control-plane/internal/integrations/genericwebhook"
	"github.com/open-cluster/oc-control-plane/internal/integrations/github"
	"github.com/open-cluster/oc-control-plane/internal/integrations/kubernetes"
	"github.com/open-cluster/oc-control-plane/internal/integrations/slack"
)

// docsRoot is the authoring source of truth for user-facing product documentation,
// published through Mintlify into the org-level docs site. The convention is permanent:
// docs/ holds MDX product pages and the site's docs.json, and nothing else — no working
// notes, no plans, no generated reasoning. Working state stays out of the tree entirely;
// version control is the archive.
var docsRoot = filepath.Join(moduleRoot, "docs")

// The site chrome, by name. Mintlify publishes docs/ directly from this repository, so the
// logo and favicon ship beside the content — and they are a NAMED list rather than "any
// .svg", which would exempt every stray diagram export and turn the gate off. A new chrome
// file has to be added here, and adding one is a decision.
var docsChrome = map[string]bool{
	".mintignore":    true,
	"favicon.svg":    true,
	"logo/light.svg": true,
	"logo/dark.svg":  true,
}

func TestDocsPublicationControlsExist(t *testing.T) {
	t.Parallel()

	for _, path := range []string{
		filepath.Join(docsRoot, ".mintignore"),
		filepath.Join(moduleRoot, "scripts", "validate-docs.mjs"),
	} {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("reading documentation publication control %s: %v", path, err)
			continue
		}
		if len(strings.TrimSpace(string(content))) == 0 {
			t.Errorf("documentation publication control %s is empty", path)
		}
	}
}

var pagesOutsidePrimaryNavigation = map[string]bool{
	"feature-availability": true,
}

// TestProductDocumentationIsMintlifyMDX holds docs/ to the published site's own files. A
// stray .md, a scratch note, or a plan dropped here would be published to the public
// documentation site, which is why the shape is a build gate rather than a review habit.
func TestProductDocumentationIsMintlifyMDX(t *testing.T) {
	t.Parallel()

	pages := 0
	err := filepath.WalkDir(docsRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, relErr := filepath.Rel(docsRoot, path)
		if relErr != nil {
			return relErr
		}
		relative = filepath.ToSlash(relative)
		switch {
		case relative == "docs.json" || docsChrome[relative]:
		case strings.HasSuffix(relative, ".mdx"):
			pages++
		default:
			t.Errorf("docs/%s is not a product documentation page; docs/ holds MDX pages, "+
				"docs.json and the named site chrome only, and working artifacts stay out "+
				"of the tree", relative)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking docs/: %v", err)
	}
	if pages == 0 {
		t.Fatal("docs/ holds no MDX pages; the gate would pass vacuously")
	}
}

func TestPublishedDocsExcludePrivateAndRetiredMaterial(t *testing.T) {
	t.Parallel()

	for page := range docsPages(t) {
		content, err := os.ReadFile(filepath.Join(docsRoot, page+".mdx"))
		if err != nil {
			t.Fatalf("reading docs/%s.mdx: %v", page, err)
		}
		text := string(content)
		for _, forbidden := range []string{
			"AGENTS.md", "plans/", "operator surface", "webhook-work",
			"OperatorHandlers", "Environment record",
		} {
			if strings.Contains(text, forbidden) {
				t.Errorf("docs/%s.mdx publishes forbidden material %q", page, forbidden)
			}
		}
	}
}

// TestDocsNavigationCoversEveryPage proves docs.json and the page tree agree in both
// directions: a navigation entry naming a page that does not exist is a broken build on
// the published site, and a page navigation never names is published-but-unreachable —
// documentation that silently lags the product, which is what this gate exists to refuse.
// Every page must also open with frontmatter carrying a title and a description, because
// the site renders both and a page without them ships looking broken.
func TestDocsNavigationCoversEveryPage(t *testing.T) {
	t.Parallel()

	referenced := navigationPages(t)
	if len(referenced) == 0 {
		t.Fatal("docs.json navigation names no pages; the gate would pass vacuously")
	}

	existing := docsPages(t)
	for page := range referenced {
		if !existing[page] {
			t.Errorf("docs.json navigation names %q, but docs/%s.mdx does not exist", page, page)
		}
	}
	for page := range existing {
		if !referenced[page] && !pagesOutsidePrimaryNavigation[page] {
			t.Errorf("docs/%s.mdx is not reachable from docs.json navigation; add it or "+
				"delete the page", page)
		}
	}

	for page := range existing {
		assertFrontmatter(t, page)
	}
}

func TestEveryInternalDocumentationLinkResolves(t *testing.T) {
	t.Parallel()

	pages := docsPages(t)
	redirects := docsRedirects(t)
	linkPattern := regexp.MustCompile(`\]\((/[^)#?]*)(?:[?#][^)]*)?\)`)
	for page := range pages {
		content, err := os.ReadFile(filepath.Join(docsRoot, page+".mdx"))
		if err != nil {
			t.Fatalf("reading docs/%s.mdx: %v", page, err)
		}
		for _, match := range linkPattern.FindAllStringSubmatch(string(content), -1) {
			target := strings.Trim(strings.TrimSuffix(match[1], "/"), "/")
			if target == "" || pages[target] || redirects["/"+target] {
				continue
			}
			t.Errorf("docs/%s.mdx links to missing internal page %q", page, match[1])
		}
	}
}

func TestQuickstartCoversTheCompleteFirstRun(t *testing.T) {
	t.Parallel()

	content, err := os.ReadFile(filepath.Join(docsRoot, "getting-started", "quickstart.mdx"))
	if err != nil {
		t.Fatalf("reading Quickstart: %v", err)
	}
	text := string(content)
	for _, required := range []string{
		"docker compose -f deploy/compose/compose.yaml up --build",
		"create the first User",
		"create an Organization",
		"Generic Webhook",
		"202 Accepted",
		"docker compose -f deploy/compose/compose.yaml down",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("Quickstart does not cover %q", required)
		}
	}
}

func TestDockerComposeGuideIsExecutable(t *testing.T) {
	t.Parallel()

	content, err := os.ReadFile(filepath.Join(docsRoot, "self-hosting", "docker-compose.mdx"))
	if err != nil {
		t.Fatalf("reading Docker Compose guide: %v", err)
	}
	text := string(content)
	for _, required := range []string{
		"deploy/compose/compose.yaml",
		"docker compose",
		"postgres_data",
		"/healthz",
		"/readyz",
		"Troubleshooting",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("Docker Compose guide does not cover %q", required)
		}
	}
}

// TestEveryProviderManifestHasAPage enforces the integration Definition of Done's
// documentation clause from the compiled provider manifests.
func TestEveryProviderManifestHasAPage(t *testing.T) {
	t.Parallel()

	keys := providerManifestKeys(t)
	if len(keys) == 0 {
		t.Fatal("no provider manifests found; the gate would pass vacuously")
	}
	pages := docsPages(t)
	for _, key := range keys {
		matches := 0
		for page := range pages {
			parts := strings.Split(page, "/")
			if len(parts) == 3 && parts[0] == "integrations" && parts[2] == key {
				matches++
			}
		}
		if matches != 1 {
			t.Errorf("integration type %q has %d documentation pages under a product-role "+
				"directory in docs/integrations; want exactly one so an operator can connect "+
				"it without reading source code", key, matches)
		}
	}
}

// TestIntegrationNavigationUsesProductRoles keeps the integration catalog scannable as
// it grows: systems are grouped by the role they play in OpenCluster, not published as a
// flat provider list or an arbitrary vendor taxonomy.
func TestDocsNavigationMatchesTheApprovedPublicPlan(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join(docsRoot, "docs.json"))
	if err != nil {
		t.Fatalf("reading docs/docs.json: %v", err)
	}
	var site map[string]any
	if err := json.Unmarshal(raw, &site); err != nil {
		t.Fatalf("docs/docs.json is not valid JSON: %v", err)
	}
	want := []struct {
		group string
		pages []string
	}{
		{"Get started", []string{"index", "getting-started/quickstart", "getting-started/set-up-opencluster", "getting-started/connect-your-tools", "getting-started/run-your-first-investigation"}},
		{"Concepts", []string{"concepts/core-concepts", "concepts/alert-events-and-incidents", "concepts/investigations-and-conversations", "concepts/results-evidence-and-actions", "concepts/postmortems"}},
		{"Integrations", []string{"integrations/overview", "integrations/alerting/generic_webhook", "integrations/alerting/alertmanager", "integrations/infrastructure/kubernetes", "integrations/source-control/github", "integrations/collaboration/slack"}},
		{"Self-hosting", []string{"self-hosting/docker-compose", "self-hosting/helm", "self-hosting/configuration", "self-hosting/model-providers-and-byok", "self-hosting/upgrade", "self-hosting/backup-and-restore", "self-hosting/troubleshooting"}},
		{"Security", []string{"security/overview", "security/data-handling", "security/authentication-and-authorization", "security/secrets-and-relay", "security/ai-provider-data-flow"}},
		{"API reference", []string{"api-reference/overview", "api-reference/authentication-and-organization-context", "api-reference/errors-and-pagination", "api-reference/webhooks", "api-reference/investigation-events", "api-reference/generated-endpoints"}},
		{"Developers", []string{"developers/architecture", "developers/local-development", "developers/contributing", "developers/repositories"}},
	}
	navigation, ok := site["navigation"].(map[string]any)
	if !ok {
		t.Fatal("docs.json navigation is not an object")
	}
	groups, ok := navigation["groups"].([]any)
	if !ok || len(groups) != len(want) {
		t.Fatalf("docs.json has %d primary groups, want %d", len(groups), len(want))
	}
	for index, expected := range want {
		group, ok := groups[index].(map[string]any)
		if !ok || group["group"] != expected.group {
			t.Errorf("navigation group %d = %v, want %q", index, group["group"], expected.group)
			continue
		}
		pages, ok := group["pages"].([]any)
		if !ok || len(pages) != len(expected.pages) {
			t.Errorf("%s has %d pages, want %d", expected.group, len(pages), len(expected.pages))
			continue
		}
		for pageIndex, page := range expected.pages {
			if expected.group == "API reference" && page == "api-reference/generated-endpoints" {
				generated, object := pages[pageIndex].(map[string]any)
				if !object || generated["group"] != "Generated endpoints" ||
					generated["root"] != page || generated["openapi"] == "" {
					t.Errorf("API reference generated endpoint entry = %v, want an OpenAPI-backed group rooted at %q", pages[pageIndex], page)
				}
				continue
			}
			if pages[pageIndex] != page {
				t.Errorf("%s page %d = %v, want %q", expected.group, pageIndex, pages[pageIndex], page)
			}
		}
	}
}

// navigationPages reports every page reference in docs.json's navigation, walking the
// structure generically so a regrouping does not need a gate change. The "global" block
// holds external anchors, not pages, and is skipped.
func navigationPages(t *testing.T) map[string]bool {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(docsRoot, "docs.json"))
	if err != nil {
		t.Fatalf("reading docs/docs.json: %v", err)
	}
	var site map[string]any
	if err := json.Unmarshal(raw, &site); err != nil {
		t.Fatalf("docs/docs.json is not valid JSON: %v", err)
	}
	navigation, ok := site["navigation"]
	if !ok {
		t.Fatal("docs/docs.json declares no navigation")
	}

	pages := map[string]bool{}
	collectPages(navigation, pages)
	return pages
}

func docsRedirects(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(docsRoot, "docs.json"))
	if err != nil {
		t.Fatalf("reading docs/docs.json: %v", err)
	}
	var site struct {
		Redirects []struct {
			Source string `json:"source"`
		} `json:"redirects"`
	}
	if err := json.Unmarshal(raw, &site); err != nil {
		t.Fatalf("docs/docs.json is not valid JSON: %v", err)
	}
	redirects := make(map[string]bool, len(site.Redirects))
	for _, redirect := range site.Redirects {
		redirects[redirect.Source] = true
	}
	return redirects
}

func collectPages(node any, pages map[string]bool) {
	switch typed := node.(type) {
	case map[string]any:
		for key, value := range typed {
			if key == "global" {
				continue
			}
			if key == "root" {
				if page, ok := value.(string); ok {
					pages[page] = true
				}
				continue
			}
			collectPages(value, pages)
		}
	case []any:
		for _, value := range typed {
			if page, ok := value.(string); ok {
				pages[page] = true
				continue
			}
			collectPages(value, pages)
		}
	}
}

// docsPages reports every MDX page under docs/ by its navigation name: the forward-slash
// relative path without the extension.
func docsPages(t *testing.T) map[string]bool {
	t.Helper()

	pages := map[string]bool{}
	err := filepath.WalkDir(docsRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".mdx") {
			return nil
		}
		relative, relErr := filepath.Rel(docsRoot, path)
		if relErr != nil {
			return relErr
		}
		pages[strings.TrimSuffix(filepath.ToSlash(relative), ".mdx")] = true
		return nil
	})
	if err != nil {
		t.Fatalf("walking docs/: %v", err)
	}
	return pages
}

func assertFrontmatter(t *testing.T, page string) {
	t.Helper()

	content, err := os.ReadFile(filepath.Join(docsRoot, page+".mdx"))
	if err != nil {
		t.Fatalf("reading docs/%s.mdx: %v", page, err)
	}
	text := strings.ReplaceAll(string(content), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		t.Errorf("docs/%s.mdx does not open with frontmatter", page)
		return
	}
	body := text[len("---\n"):]
	end := strings.Index(body, "\n---\n")
	if end < 0 {
		t.Errorf("docs/%s.mdx frontmatter never closes", page)
		return
	}
	frontmatter := body[:end]
	for _, field := range []string{"title:", "description:"} {
		found := false
		for _, line := range strings.Split(frontmatter, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), field) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("docs/%s.mdx frontmatter lacks %q; the site renders both the title "+
				"and the description", page, strings.TrimSuffix(field, ":"))
		}
	}
}

func providerManifestKeys(t *testing.T) []string {
	t.Helper()
	catalog, err := integrations.NewCatalog(
		alertmanager.Definition(),
		kubernetes.Definition(),
		slack.Definition(slack.NewClient(""), nil, false),
		github.Definition(nil, github.NewClient("")),
		genericwebhook.Definition(),
	)
	if err != nil {
		t.Fatalf("assembling provider manifests: %v", err)
	}
	manifests := catalog.Manifests()
	keys := make([]string, 0, len(manifests))
	for _, manifest := range manifests {
		if manifest.Key == "" {
			t.Fatal("provider manifest has an empty key")
		}
		wantSlug := "integrations/" + string(manifest.Category) + "/" + manifest.Key
		if manifest.DocumentationSlug != wantSlug {
			t.Errorf("provider manifest %q documentation slug = %q, want %q",
				manifest.Key, manifest.DocumentationSlug, wantSlug)
		}
		keys = append(keys, manifest.Key)
	}
	return keys
}
