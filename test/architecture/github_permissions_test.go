package gates_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/integrations/github"
)

// The grant a customer approves is described in two places: the permission map the GitHub
// provider declares, and the table the product documentation publishes. A security team
// reads the second and this build obeys the first, so the two disagreeing is worse than
// either being wrong alone — the documentation would be a promise the code does not keep.
//
// This gate is what makes them one statement. It reads the published table out of the page
// and compares it against the union of the map, in both directions.

// githubPage is the published page whose permission table this gate holds.
var githubPage = filepath.Join(moduleRoot, "docs", "integrations", "source-control", "github.mdx")

func TestTheDocumentedGitHubPermissionsAreExactlyTheOnesTheToolsNeed(t *testing.T) {
	t.Parallel()

	published := publishedPermissions(t)
	if len(published) == 0 {
		t.Fatal("the GitHub page publishes no permission table; the gate would pass vacuously")
	}

	requested := map[string]bool{}
	for _, permission := range github.RequestedPermissions() {
		requested[string(permission)] = true
	}

	for permission := range published {
		if !requested[permission] {
			t.Errorf("the documentation states %q and no shipped tool needs it; a "+
				"permission we cannot map to a tool should not be asked for, and telling a "+
				"customer we ask for it is worse", permission)
		}
	}
	for permission := range requested {
		if !published[permission] {
			t.Errorf("the tools need %q and the documentation does not state it; a "+
				"customer's security team would approve a grant the page does not describe",
				permission)
		}
	}
}

// No write permission is requested for any resource, and this reads the published table
// rather than the map so that a documentation page cannot quietly describe one.
func TestTheDocumentedGitHubGrantIsReadOnly(t *testing.T) {
	t.Parallel()

	for _, line := range permissionTableRows(t) {
		columns := tableColumns(line)
		if len(columns) < 2 {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(columns[1]), "Read") {
			t.Errorf("the GitHub page grants %q %q; this integration reads and never writes",
				columns[0], columns[1])
		}
	}
}

// publishedPermissions is the first column of the page's permission table.
func publishedPermissions(t *testing.T) map[string]bool {
	t.Helper()

	published := map[string]bool{}
	for _, line := range permissionTableRows(t) {
		columns := tableColumns(line)
		if len(columns) < 2 {
			continue
		}
		published[strings.TrimSpace(columns[0])] = true
	}
	return published
}

// permissionTableRows returns the body rows of the "GitHub App permissions" table. The
// section heading anchors the read, so another table on the page — or the same table moved
// — fails the gate rather than being silently skipped.
func permissionTableRows(t *testing.T) []string {
	t.Helper()

	body, err := os.ReadFile(githubPage)
	if err != nil {
		t.Fatalf("reading %s: %v", githubPage, err)
	}
	_, after, found := strings.Cut(string(body), "## GitHub App permissions")
	if !found {
		t.Fatalf("%s has no GitHub App permissions section; the published grant is what "+
			"a customer's security team reads", githubPage)
	}

	var rows []string
	for _, line := range strings.Split(after, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "##") {
			break
		}
		if !strings.HasPrefix(trimmed, "|") || strings.Contains(trimmed, "---") {
			continue
		}
		if strings.Contains(trimmed, "Repository permission") {
			continue
		}
		rows = append(rows, trimmed)
	}
	return rows
}

func tableColumns(row string) []string {
	return strings.Split(strings.Trim(strings.TrimSpace(row), "|"), "|")
}
