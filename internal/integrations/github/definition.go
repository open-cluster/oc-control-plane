package github

import (
	"context"
	"errors"
	"strconv"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// The Tools connecting GitHub makes available. All reads; no write path to
// anyone's code exists in this build, by decision rather than by omission.
const (
	toolListRepositories = "github.list_repositories"
	toolReadCommits      = "github.read_commits"
	toolReadCommit       = "github.read_commit"
	toolReadPullRequest  = "github.read_pull_request"
	toolReadWorkflowRuns = "github.read_workflow_runs"
	toolReadJobLog       = "github.read_job_log"
	toolReadFile         = "github.read_file"
	toolListReleases     = "github.list_releases"
)

func Definition(app *App, client *Client) integrations.Definition {
	where := deployment{app: app, client: client}
	return integrations.Definition{
		Manifest: integrations.Manifest{
			Type: integrations.TypeGitHub, Key: "github", Name: "GitHub",
			Description: "Give investigations read-only access to selected repositories for " +
				"commits, pull requests, CI failures, files, and releases.",
			Logo: "github", Category: integrations.CategorySourceControl,
			SourceURL:         "https://docs.github.com/apps/using-github-apps/installing-a-github-app-from-a-third-party",
			DocumentationSlug: "integrations/source-control/github",
			Tools:             tools(app, client),
		},
		Probe: func(ctx context.Context, input integrations.ProbeInput) integrations.Verification {
			installation, err := installationOf(input.Integration)
			if err != nil {
				return integrations.Verification{
					Status: integrations.StatusFailed,
					Note:   "the integration carries no usable installation identity; reconnect it and verify again",
				}
			}
			// What the last run established travels in: an installation GitHub has
			// stopped serving is a removal when this deployment verified it before,
			// and an unknown id when it never did.
			return probe(ctx, where, installation)
		},
		Connect: connect(app, client),
	}
}

func installationOf(integration integrations.Integration) (int64, error) {
	if integration.Installation == nil {
		return 0, errors.New("installationId is not a whole positive number")
	}
	id, err := strconv.ParseInt(integration.Installation.Workspace, 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("installationId is not a whole positive number")
	}
	return id, nil
}
