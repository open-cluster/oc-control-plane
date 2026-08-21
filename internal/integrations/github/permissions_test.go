package github

import (
	"strings"
	"testing"
)

// The permission map is only worth having if it cannot drift from the tools it maps. These
// hold it to the declared set in both directions and prove the union is what the App
// registration is asked to request.

func TestEveryToolIsMappedToItsEndpointsAndPermissions(t *testing.T) {
	t.Parallel()

	mapped := make(map[string]toolGrant, len(toolGrants))
	for _, grant := range toolGrants {
		if _, twice := mapped[grant.Tool]; twice {
			t.Errorf("%s is mapped twice; one entry is the map and a second is a guess",
				grant.Tool)
		}
		mapped[grant.Tool] = grant
	}

	declared := make(map[string]bool)
	for _, tool := range tools(nil, NewClient("")) {
		declared[tool.Name] = true
		grant, found := mapped[tool.Name]
		if !found {
			t.Errorf("%s calls github and names no endpoint or permission; an unmapped "+
				"tool is a grant nobody can audit", tool.Name)
			continue
		}
		if len(grant.Endpoints) == 0 || len(grant.Permissions) == 0 {
			t.Errorf("%s is mapped to %d endpoints and %d permissions; both must be named",
				tool.Name, len(grant.Endpoints), len(grant.Permissions))
		}
		for _, endpoint := range grant.Endpoints {
			if !strings.HasPrefix(endpoint, "GET ") {
				t.Errorf("%s names %q; every endpoint this build calls is a read",
					tool.Name, endpoint)
			}
		}
	}

	for tool := range mapped {
		if !declared[tool] {
			t.Errorf("the map names %s, which this build does not declare; a permission "+
				"justified by a tool that does not exist is a permission asked for nothing",
				tool)
		}
	}
}

// The union is what the App registration requests. A permission needed by the map and
// missing from the request order would leave it silently, which is exactly the drift the
// union exists to prevent.
func TestTheRequestedPermissionsAreTheUnionOfTheMap(t *testing.T) {
	t.Parallel()

	needed := map[AppPermission]bool{}
	for _, grant := range toolGrants {
		for _, permission := range grant.Permissions {
			needed[permission] = true
		}
	}
	requested := RequestedPermissions()
	if len(requested) != len(needed) {
		t.Fatalf("the map needs %d permissions and %d are requested: %v",
			len(needed), len(requested), requested)
	}
	for _, permission := range requested {
		if !needed[permission] {
			t.Errorf("%s is requested and no tool needs it; a permission we cannot map to "+
				"a shipped tool is one we should not ask for", permission)
		}
	}
}

// A tool's operator-facing permission line is rendered from the map, so the line and the
// endpoints cannot disagree. Reading a pull request needs Pull requests, not Contents —
// which is what the hand-written line used to say.
func TestAToolsPermissionLineIsRenderedFromItsMapping(t *testing.T) {
	t.Parallel()

	lines := map[string]string{}
	for _, tool := range tools(nil, NewClient("")) {
		lines[tool.Name] = tool.Permissions
	}

	pullRequest := lines["github.read_pull_request"]
	if !strings.Contains(pullRequest, string(PermissionPullRequests)) ||
		!strings.Contains(pullRequest, string(PermissionChecks)) {
		t.Errorf("github.read_pull_request states %q, which is not what its endpoints need",
			pullRequest)
	}
	if strings.Contains(pullRequest, string(PermissionContents)) {
		t.Errorf("github.read_pull_request still claims Contents: %q", pullRequest)
	}
	for name, line := range lines {
		if strings.Contains(line, "unmapped") {
			t.Errorf("%s renders %q; its entry is missing from the map", name, line)
		}
		if !strings.Contains(line, "read-only") {
			t.Errorf("%s states %q without saying the access is read-only", name, line)
		}
	}
}
