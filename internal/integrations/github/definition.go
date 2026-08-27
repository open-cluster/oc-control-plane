package github

import (
	"context"
	"errors"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// The Tools connecting GitHub makes available. All reads; no write path to
// anyone's code exists in this build, by decision rather than by omission.
const (
	ListRepositories = "github.list_repositories"
	ReadCommits      = "github.read_commits"
	ReadCommit       = "github.read_commit"
	ReadPullRequest  = "github.read_pull_request"
	ReadWorkflowRuns = "github.read_workflow_runs"
	ReadJobLog       = "github.read_job_log"
	ReadFile         = "github.read_file"
	ListReleases     = "github.list_releases"
)

// Definition is what this provider exports to the catalog. Metadata mirrors the seeded
// integration_type row; a test proves the two agree.
//
// The App may be nil — a deployment that configured none still serves github in the
// catalog, because the compiled provider set and the seeded reference rows must agree
// exactly; connecting is what fails, live and with the reason, when the probe runs.
func Definition(app *App, client *Client) integrations.Definition {
	where := deployment{
		app: app, client: client, webURL: browserOrigin(client),
	}
	return integrations.Definition{
		ID:   integrations.TypeGitHub,
		Key:  "github",
		Name: "GitHub",
		Description: "Give investigations read-only access to selected repositories for " +
			"commits, pull requests, CI failures, files, and releases.",
		Logo:     "github",
		Category: integrations.CategorySourceControl,
		DocumentationURL: "https://docs.github.com/apps/using-github-apps/" +
			"installing-a-github-app-from-a-third-party",
		// The configuration form is the fallback, not the front door. Where this
		// deployment registered an installation flow, a customer presses Connect and
		// never sees this field; where it did not, this is how GitHub is connected.
		Config: []integrations.Field{
			{
				Name:  "installationId",
				Title: "Installation ID",
				Description: "The numeric id of the app installation on your account, " +
					"from the installation's settings page URL. Only needed where this " +
					"deployment offers no one-click installation. Access follows the " +
					"repositories that installation selects, by stable ids that survive " +
					"renames.",
				Type:     integrations.FieldInteger,
				Required: true,
			},
		},
		RequiresRelay:    false,
		ReceivesWebhooks: false,
		Probe: func(ctx context.Context, input integrations.ProbeInput) integrations.Verification {
			installation, err := installationOf(input.Integration)
			if err != nil {
				return integrations.Verification{
					Status: integrations.StatusFailed,
					Note:   "the integration carries no usable installation id; set installationId and verify again",
				}
			}
			// What the last run established travels in: an installation GitHub has
			// stopped serving is a removal when this deployment verified it before,
			// and an unknown id when it never did.
			return probe(ctx, where, installation, input.Integration.VerifyFacts)
		},
		Tools: tools(app, client),
	}
}

// installationOf reads the installation id an Integration is configured with.
func installationOf(integration integrations.Integration) (int64, error) {
	id, err := wholePositiveID(integration.Configuration["installationId"])
	if err != nil {
		return 0, errors.New("installationId is not a whole positive number")
	}
	return id, nil
}
