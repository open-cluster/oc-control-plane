package gates_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Directories holding this repository's own documentation. Vendored agent skills and
// generated output are excluded: they are not maintained here, so a reference inside them
// is not this repository's to keep correct.
var documentationRoots = []string{"CONTEXT.md", "README.md", "docs", "plans"}

// A document that lives beside the frozen .NET implementation is cited by writing its path
// followed by this marker on the same line. The marker is what separates a deliberate
// pointer into the historical record from a path that simply no longer resolves — without
// it the two are indistinguishable, which is the confusion this gate exists to prevent.
const frozenRecordMarker = "in the frozen .NET reference repository"

// Documentation cites documentation two ways: as a Markdown link target, and as a
// backtick-quoted repository-relative path, which is what most of these documents use.
var (
	markdownLink   = regexp.MustCompile(`\[[^\]]*\]\(([^)]+)\)`)
	backtickedPath = regexp.MustCompile("`([^`\n]+)`")
)

// A reference that no longer resolves breaks nothing and fails no build. It misleads
// whoever follows it, months later, with no signal that anything is wrong — and review does
// not catch it, because a reviewer sees the document being edited rather than the documents
// pointing at it. Moving or renaming a document is cheap; updating everything that cites it
// is what gets forgotten.
//
// The gate covers references to Markdown documents only. Prose naming a package or a
// directory is left alone, because telling those apart from ordinary quoted text produces
// false positives, and document-to-document citation is the failure this repository
// actually creates.
func TestEveryDocumentReferenceResolves(t *testing.T) {
	t.Parallel()

	documents := documentationFiles(t)
	if len(documents) == 0 {
		t.Fatal("no documentation found; the gate would pass vacuously")
	}

	checked := 0
	for _, document := range documents {
		content, err := os.ReadFile(filepath.Join(moduleRoot, document))
		if err != nil {
			t.Fatalf("reading %s: %v", document, err)
		}

		for lineNumber, line := range strings.Split(string(content), "\n") {
			if strings.Contains(line, frozenRecordMarker) {
				continue
			}
			for _, reference := range documentReferences(line) {
				checked++
				if _, err := os.Stat(filepath.Join(moduleRoot, reference)); err != nil {
					t.Errorf("%s line %d cites %s, which does not exist here; correct the "+
						"path, or mark it %q if it belongs to the frozen record",
						document, lineNumber+1, reference, frozenRecordMarker)
				}
			}
		}
	}

	if checked == 0 {
		t.Fatal("no document references found; the gate would pass vacuously")
	}
}

// documentReferences reports the repository-relative Markdown documents one line cites.
func documentReferences(line string) []string {
	var references []string
	for _, match := range markdownLink.FindAllStringSubmatch(line, -1) {
		references = appendDocumentReference(references, match[1])
	}
	for _, match := range backtickedPath.FindAllStringSubmatch(line, -1) {
		references = appendDocumentReference(references, match[1])
	}
	return references
}

// appendDocumentReference keeps only what names a Markdown document in this repository.
// Absolute URLs belong to somewhere else, and anchors address a position within a page.
func appendDocumentReference(references []string, candidate string) []string {
	candidate = strings.TrimSpace(candidate)
	candidate, _, _ = strings.Cut(candidate, "#")
	switch {
	case !strings.HasSuffix(candidate, ".md"):
		return references
	case strings.Contains(candidate, "://"), strings.HasPrefix(candidate, "/"):
		return references
	case strings.Contains(candidate, " "):
		return references
	}
	return append(references, strings.TrimPrefix(candidate, "./"))
}

// documentationFiles reports every Markdown document this repository maintains, as paths
// relative to the module root.
func documentationFiles(t *testing.T) []string {
	t.Helper()

	var documents []string
	for _, root := range documentationRoots {
		absolute := filepath.Join(moduleRoot, root)
		info, err := os.Stat(absolute)
		if err != nil {
			t.Fatalf("documentation root %s is missing: %v", root, err)
		}
		if !info.IsDir() {
			documents = append(documents, root)
			continue
		}
		err = filepath.WalkDir(absolute, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
				return nil
			}
			relative, relErr := filepath.Rel(moduleRoot, path)
			if relErr != nil {
				return relErr
			}
			documents = append(documents, filepath.ToSlash(relative))
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", root, err)
		}
	}
	return documents
}
