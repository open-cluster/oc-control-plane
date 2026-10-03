package github

import "strings"

type AppPermission string

const (
	PermissionMetadata     AppPermission = "Metadata"
	PermissionContents     AppPermission = "Contents"
	PermissionPullRequests AppPermission = "Pull requests"
	PermissionChecks       AppPermission = "Checks"
	PermissionActions      AppPermission = "Actions"
)

type toolGrant struct {
	Tool        string
	Endpoints   []string
	Permissions []AppPermission
}

const resolveRepository = "GET /installation/repositories"

var toolGrants = []toolGrant{
	{
		Tool:        toolListRepositories,
		Endpoints:   []string{resolveRepository},
		Permissions: []AppPermission{PermissionMetadata},
	},
	{
		Tool:        toolReadCommits,
		Endpoints:   []string{resolveRepository, "GET /repos/{owner}/{repo}/commits"},
		Permissions: []AppPermission{PermissionMetadata, PermissionContents},
	},
	{
		Tool:        toolReadCommit,
		Endpoints:   []string{resolveRepository, "GET /repos/{owner}/{repo}/commits/{sha}"},
		Permissions: []AppPermission{PermissionMetadata, PermissionContents},
	},
	{
		Tool: toolReadPullRequest,
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
		Tool:        toolReadWorkflowRuns,
		Endpoints:   []string{resolveRepository, "GET /repos/{owner}/{repo}/actions/runs"},
		Permissions: []AppPermission{PermissionMetadata, PermissionActions},
	},
	{
		Tool: toolReadJobLog,
		Endpoints: []string{
			resolveRepository,
			"GET /repos/{owner}/{repo}/actions/runs/{run}/jobs",
			"GET /repos/{owner}/{repo}/actions/jobs/{job}/logs",
		},
		Permissions: []AppPermission{PermissionMetadata, PermissionActions},
	},
	{
		Tool:        toolReadFile,
		Endpoints:   []string{resolveRepository, "GET /repos/{owner}/{repo}/contents/{path}"},
		Permissions: []AppPermission{PermissionMetadata, PermissionContents},
	},
	{
		Tool:        toolListReleases,
		Endpoints:   []string{resolveRepository, "GET /repos/{owner}/{repo}/releases"},
		Permissions: []AppPermission{PermissionMetadata, PermissionContents},
	},
}

var requestOrder = []AppPermission{
	PermissionMetadata, PermissionContents, PermissionPullRequests,
	PermissionChecks, PermissionActions,
}

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

func permissionProse(tool string) string {
	for _, grant := range toolGrants {
		if grant.Tool != tool {
			continue
		}
		return "read-only " + english(grant.Permissions) +
			", within the repositories this installation selected"
	}
	return "unmapped: this tool declares no endpoints and no permissions"
}

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
