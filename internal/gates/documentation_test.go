package gates_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Directories whose Markdown is not maintained here: vendored agent skills, generated
// output, and dependencies. A reference inside them is not this repository's to keep correct.
var unmaintainedDirectories = map[string]bool{
	".git": true, ".claude": true, ".agents": true, ".codex": true,
	"gen": true, "node_modules": true, "vendor": true,
}

// A document that lives beside the frozen .NET implementation is cited by writing its path
// followed immediately by this marker. The marker is what separates a deliberate pointer
// into the historical record from a path that simply no longer resolves — without it the
// two are indistinguishable, which is the confusion this gate exists to prevent.
const frozenRecordMarker = "in the frozen .NET reference repository"

// The sibling repositories this product is built from. A citation whose first segment names one
// is a cross-repository reference: those documents exist, they are maintained, and they are not
// in this checkout — so requiring them to resolve here would mean either deleting a correct
// reference or checking out four repositories to run a test.
//
// It is a NAMED list rather than "anything with a slash that does not resolve", which would
// exempt every broken reference and turn the gate off. A new sibling has to be added here, and
// adding one is a decision.
var siblingRepositories = map[string]bool{
	"oc-frontend": true, "oc-relay": true, "oc-control-plane": true,
	"opencluster-web": true, "opencluster-relay": true,
}

// Any path-like token naming a Markdown document, in prose, in backticks, or as a link
// target, so the gate does not depend on which citation syntax an author reached for.
//
// A leading "./" is stripped before the path is resolved. Without that, a self-reference
// written the way a Markdown link is ordinarily written reads as a directory named "." and
// fails, which is the gate being wrong rather than the citation.
//
// A directory component is required. A bare filename is as likely to be a naming convention
// as a citation — a decision record describing `ADR-NNN-slug.md` names a pattern, not a
// file — and nothing distinguishes the two reliably. Bare filenames are therefore not
// checked, which is a deliberate hole rather than an oversight.
var documentPath = regexp.MustCompile(`(?:[\w.@-]+/)+[\w.@-]+\.md`)

// A reference that no longer resolves breaks nothing and fails no build. It misleads
// whoever follows it, months later, with no signal that anything is wrong — and review does
// not catch it, because a reviewer sees the document being edited rather than the documents
// pointing at it. Moving or renaming a document is cheap; updating everything that cites it
// is what gets forgotten.
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
		for number, line := range strings.Split(string(content), "\n") {
			for _, reference := range unmarkedReferences(line) {
				checked++
				if !documentExists(reference) {
					t.Errorf("%s line %d cites %s, which does not exist here; correct the "+
						"path, or follow it with %q if it belongs to the frozen record",
						document, number+1, reference, frozenRecordMarker)
				}
			}
		}
	}

	if checked == 0 {
		t.Fatal("no document references found; the gate would pass vacuously")
	}
}

// unmarkedReferences reports the documents one line cites, excluding those explicitly marked
// as belonging to the frozen record. The marker exempts only the reference it directly
// follows, so an unrelated broken reference sharing the line is still reported.
func unmarkedReferences(line string) []string {
	var references []string
	for _, position := range documentPath.FindAllStringIndex(line, -1) {
		following := strings.TrimLeft(line[position[1]:], "`) ")
		if strings.HasPrefix(following, frozenRecordMarker) {
			continue
		}
		references = append(references, line[position[0]:position[1]])
	}
	return references
}

// documentExists reports whether a reference names a file that exists, matching the case it
// was written in. os.Stat alone is case-insensitive on Windows and case-sensitive on Linux,
// where CI runs, so a mis-cased reference would otherwise pass locally and fail there.
func documentExists(reference string) bool {
	segments := strings.Split(strings.TrimPrefix(reference, "./"), "/")
	if siblingRepositories[segments[0]] {
		return true
	}

	directory := moduleRoot
	for index, segment := range segments {
		entries, err := os.ReadDir(directory)
		if err != nil {
			return false
		}
		if !containsEntry(entries, segment, index == len(segments)-1) {
			return false
		}
		directory = filepath.Join(directory, segment)
	}
	return true
}

func containsEntry(entries []os.DirEntry, name string, mustBeFile bool) bool {
	for _, entry := range entries {
		if entry.Name() == name {
			return !mustBeFile || !entry.IsDir()
		}
	}
	return false
}

// documentationFiles reports every Markdown document this repository maintains, as
// forward-slash paths relative to the module root. It walks the whole tree rather than a
// list of known locations, so a document added somewhere new is covered without anyone
// remembering to extend a list.
func documentationFiles(t *testing.T) []string {
	t.Helper()

	var documents []string
	err := filepath.WalkDir(moduleRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if unmaintainedDirectories[entry.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".md") {
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
		t.Fatalf("walking the repository: %v", err)
	}
	return documents
}
