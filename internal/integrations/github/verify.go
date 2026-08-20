package github

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// probe verifies one installation live: GitHub is asked for the installation under the
// App's own credential, a token is minted under it, and the selected repositories are
// listed. It is the one place "verified" is decided for this provider — the far end
// answered as this account with these repositories, or it did not, and the note says
// which.
func probe(
	ctx context.Context, app *App, client *Client, installation int64,
) integrations.Verification {
	if !app.Configured() {
		return integrations.Verification{
			Status: integrations.StatusFailed,
			Note: "this deployment has no GitHub App configured, so it cannot reach " +
				"github; ask whoever operates it to configure one",
		}
	}

	jwt, err := app.jwt(time.Now())
	if err != nil {
		return integrations.Verification{
			Status: integrations.StatusFailed,
			Note:   "the deployment's GitHub App credential could not sign a request",
		}
	}
	found, err := client.Installation(ctx, jwt, installation)
	if err != nil {
		return judgeFailure(err, installation)
	}
	if found.Suspended {
		return integrations.Verification{
			Status: integrations.StatusFailed,
			Note: "installation " + strconv.FormatInt(installation, 10) + " for " +
				found.Account + " is suspended in github; unsuspend it and verify again",
		}
	}

	token, err := app.installationToken(ctx, installation)
	if err != nil {
		return judgeFailure(err, installation)
	}
	granted, err := client.Repositories(ctx, token, repositoryProbePage, 1)
	if err != nil {
		return judgeFailure(err, installation)
	}
	if len(granted.Repositories) == 0 {
		return integrations.Verification{
			Status: integrations.StatusDegraded,
			Note: "github answered for " + found.Account + " and the installation grants " +
				"no repositories; give it at least one in the app's settings",
		}
	}

	count := strconv.Itoa(len(granted.Repositories))
	if granted.Truncated {
		count = "at least " + count
	}
	return integrations.Verification{
		Status: integrations.StatusActive,
		Note: "verified against " + found.Account + " (" + found.AccountType + "); " +
			count + " repositories granted by " + found.RepositorySelection + " selection",
	}
}

// repositoryProbePage bounds how many repositories a verification lists. The point is
// proving the grant works and saying roughly how wide it is, not inventorying it.
const repositoryProbePage = 100

// judgeFailure turns what went wrong into the operator's answer. An unknown installation,
// a refused App credential, a rate limit and an unreachable vendor are four different
// facts, and each gets its own words.
func judgeFailure(err error, installation int64) integrations.Verification {
	var refusal *APIError
	switch {
	case errors.As(err, &refusal) && refusal.Status == http.StatusNotFound:
		return integrations.Verification{
			Status: integrations.StatusFailed,
			Note: "github does not know installation " +
				strconv.FormatInt(installation, 10) +
				" for this app; check the id, or reinstall the app on the account",
		}
	case errors.As(err, &refusal) && refusal.Status == http.StatusUnauthorized:
		return integrations.Verification{
			Status: integrations.StatusFailed,
			Note:   "github refused this deployment's app credential; its key or app id is wrong",
		}
	case errors.As(err, &refusal):
		return integrations.Verification{
			Status: integrations.StatusFailed,
			Note:   "github refused the check: " + refusal.Message,
		}
	case errors.Is(err, ErrRateLimited):
		return integrations.Verification{
			Status: integrations.StatusDegraded,
			Note:   "github is rate limiting this app; verify again in a few minutes",
		}
	default:
		return integrations.Verification{
			Status: integrations.StatusFailed,
			Note:   "github could not be reached from this deployment; check its network path and verify again",
		}
	}
}
