package github

import "strings"

// THE PERMISSION MAP: every tool, the GitHub endpoints it calls, and the App permission
// each of those endpoints requires.
//
// It exists because "the grant is minimal" is otherwise a claim nobody can check. With the
// map, the App registration asks for exactly the union below, the documentation states the
// same set, each tool's operator-facing permission line is RENDERED from it rather than
// written beside it, and a gate compares the union against the published table. A
// permission that no shipped tool justifies cannot survive that, and neither can a tool
// whose stated permissions are not the ones its endpoints need.
//
// Everything here is read. No write permission is requested for any resource, and there is
// no code path in this build that could use one.

// AppPermission is one repository permission the App registration requests, spelled as
// GitHub's own settings page spells it.
type AppPermission string

const (
	// PermissionMetadata is what lists the installation's repositories and resolves a
	// stable repository id back to the owner/name the REST routes are addressed by.
	// GitHub requires it of every App.
	PermissionMetadata AppPermission = "Metadata"
	// PermissionContents covers commits, diffs, file contents and releases.
	PermissionContents AppPermission = "Contents"
	// PermissionPullRequests covers a pull request and the files it changed.
	PermissionPullRequests AppPermission = "Pull requests"
	// PermissionChecks covers the check results reported against a commit.
	PermissionChecks AppPermission = "Checks"
	// PermissionActions covers workflow runs, their jobs, and the job logs.
	PermissionActions AppPermission = "Actions"
)

// toolGrant is one tool's entry in the map.
type toolGrant struct {
	Tool string
	// Endpoints are the GitHub REST routes this tool reaches, in the order it reaches
	// them. Every repository-addressed route is preceded by the installation's own
	// repository listing, because a repository is stored by stable id and the documented
	// routes take owner/name.
	Endpoints []string
	// Permissions are what those endpoints require, read-only.
	Permissions []AppPermission
}

// resolveRepository is the listing every repository-addressed read resolves its stable id
// through. Named once because it is the same call in seven entries and its permission is
// the reason Metadata is requested at all.
const resolveRepository = "GET /installation/repositories"

// toolGrants is the map itself. A tool absent from it, or an entry naming a tool this
// build does not declare, fails the provider's own test.
var toolGrants = []toolGrant{
	{
		Tool:        "github.list_repositories",
		Endpoints:   []string{resolveRepository},
		Permissions: []AppPermission{PermissionMetadata},
	},
	{
		Tool:        "github.read_commits",
		Endpoints:   []string{resolveRepository, "GET /repos/{owner}/{repo}/commits"},
		Permissions: []AppPermission{PermissionMetadata, PermissionContents},
	},
	{
		Tool:        "github.read_commit",
		Endpoints:   []string{resolveRepository, "GET /repos/{owner}/{repo}/commits/{sha}"},
		Permissions: []AppPermission{PermissionMetadata, PermissionContents},
	},
	{
		Tool: "github.read_pull_request",
		Endpoints: []string{
			resolveRepository,
			"GET /repos/{owner}/{repo}/pulls/{number}",
			"GET /repos/{owner}/{repo}/pulls/{number}/files",
			"GET /repos/{owner}/{repo}/commits/{sha}/check-runs",
		},
		Permissions: []AppPermission{
			PermissionMetadata, PermissionPullRequests, PermissionChecks,
		},
	},
	{
		Tool:        "github.read_workflow_runs",
		Endpoints:   []string{resolveRepository, "GET /repos/{owner}/{repo}/actions/runs"},
		Permissions: []AppPermission{PermissionMetadata, PermissionActions},
	},
	{
		Tool: "github.read_job_log",
		Endpoints: []string{
			resolveRepository,
			"GET /repos/{owner}/{repo}/actions/runs/{run}/jobs",
			"GET /repos/{owner}/{repo}/actions/jobs/{job}/logs",
		},
		Permissions: []AppPermission{PermissionMetadata, PermissionActions},
	},
	{
		Tool:        "github.read_file",
		Endpoints:   []string{resolveRepository, "GET /repos/{owner}/{repo}/contents/{path}"},
		Permissions: []AppPermission{PermissionMetadata, PermissionContents},
	},
	{
		Tool:        "github.list_releases",
		Endpoints:   []string{resolveRepository, "GET /repos/{owner}/{repo}/releases"},
		Permissions: []AppPermission{PermissionMetadata, PermissionContents},
	},
}

// requestOrder is how GitHub's own settings page orders these, so the published table and
// the registration screen read alike. A permission the map needs and this list forgets
// would silently leave the union, which the provider's own test refuses.
var requestOrder = []AppPermission{
	PermissionMetadata, PermissionContents, PermissionPullRequests,
	PermissionChecks, PermissionActions,
}

// RequestedPermissions is the union of the map: exactly what the App registration asks for
// and exactly what the documentation states.
func RequestedPermissions() []AppPermission {
	needed := map[AppPermission]bool{}
	for _, grant := range toolGrants {
		for _, permission := range grant.Permissions {
			needed[permission] = true
		}
	}
	union := make([]AppPermission, 0, len(needed))
	for _, permission := range requestOrder {
		if needed[permission] {
			union = append(union, permission)
		}
	}
	return union
}

// permissionProse is a tool's operator-facing permission line, rendered from the map so it
// cannot drift from the endpoints the tool actually calls. It never reaches the model,
// which routes by the composed description instead.
func permissionProse(tool string) string {
	for _, grant := range toolGrants {
		if grant.Tool != tool {
			continue
		}
		return "read-only " + english(grant.Permissions) +
			", within the repositories this installation selected"
	}
	// A tool with no entry is refused by the provider's own test, so this is unreachable
	// in a build that passes. It says so rather than reading as a granted permission.
	return "unmapped: this tool declares no endpoints and no permissions"
}

// english joins the permission names the way a sentence does.
func english(permissions []AppPermission) string {
	names := make([]string, 0, len(permissions))
	for _, permission := range permissions {
		names = append(names, string(permission))
	}
	switch len(names) {
	case 0:
		return "nothing"
	case 1:
		return names[0]
	default:
		return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	}
}
