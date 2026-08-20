package gates_test

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
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
	"favicon.svg":    true,
	"logo/light.svg": true,
	"logo/dark.svg":  true,
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
		if !referenced[page] {
			t.Errorf("docs/%s.mdx is not reachable from docs.json navigation; add it or "+
				"delete the page", page)
		}
	}

	for page := range existing {
		assertFrontmatter(t, page)
	}
}

// TestEverySeededIntegrationTypeHasAPage enforces the integration Definition of Done's
// documentation clause: an integration type this build seeds is one an operator can
// connect, and connecting one correctly must never require reading source code. The keys
// come from the seed migrations rather than the compiled catalog because only the
// composition root may assemble every provider.
func TestEverySeededIntegrationTypeHasAPage(t *testing.T) {
	t.Parallel()

	keys := seededIntegrationTypeKeys(t)
	if len(keys) == 0 {
		t.Fatal("no seeded integration types found; the gate would pass vacuously")
	}
	for _, key := range keys {
		// documentExists matches the case a reference was written in, because os.Stat
		// alone is case-insensitive on Windows and case-sensitive on Linux, where CI runs.
		if !documentExists("docs/integrations/" + key + ".mdx") {
			t.Errorf("integration type %q has no documentation page at docs/integrations/%s.mdx; "+
				"an integration is complete only when an operator can connect it without "+
				"reading source code", key, key)
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

func collectPages(node any, pages map[string]bool) {
	switch typed := node.(type) {
	case map[string]any:
		for key, value := range typed {
			if key == "global" {
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

// seededIntegrationTypeKeys parses the type keys out of the seed migrations' INSERT
// statements. The tuple shape — a numeric id followed by the quoted key — is what the
// existing seed rows use, and the drift test already holds those rows identical to the
// compiled catalog, so parsing the migration is parsing the catalog.
func seededIntegrationTypeKeys(t *testing.T) []string {
	t.Helper()

	migrations := filepath.Join(moduleRoot, "internal", "storage", "migrations")
	entries, err := os.ReadDir(migrations)
	if err != nil {
		t.Fatalf("reading migrations: %v", err)
	}

	statement := regexp.MustCompile(`(?s)INSERT INTO integration_type\b.*?;`)
	tuple := regexp.MustCompile(`\(\s*\d+\s*,\s*'([a-z0-9_-]+)'`)

	var keys []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		content, readErr := os.ReadFile(filepath.Join(migrations, entry.Name()))
		if readErr != nil {
			t.Fatalf("reading %s: %v", entry.Name(), readErr)
		}
		for _, insert := range statement.FindAllString(string(content), -1) {
			for _, match := range tuple.FindAllStringSubmatch(insert, -1) {
				keys = append(keys, match[1])
			}
		}
	}
	return keys
}
