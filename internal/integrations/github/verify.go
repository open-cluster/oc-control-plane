package github

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

const defaultWebURL = "https://github.com"

type deployment struct {
	app    *App
	client *Client
}

func browserOrigin(client *Client) string {
	if client == nil {
		return ""
	}
	if client.reachesTheVendorsOwnAPI() {
		return defaultWebURL
	}
	parsed, err := url.Parse(client.baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}

// judged is one verification plus the closed word the counter attributes it with. The
// reason is this build's own vocabulary and never anything GitHub sent: a value an
// outsider could choose would mint an unbounded number of time series.
type judged struct {
	integrations.Verification
	reason string
}

// probe is the single owner of GitHub Verification and records its bounded outcome metric.
func probe(ctx context.Context, where deployment, installation int64) integrations.Verification {
	outcome := judge(ctx, where, installation)
	countVerification(ctx, outcome)
	return outcome.Verification
}

func judge(ctx context.Context, where deployment, installation int64) judged {
	app, client := where.app, where.client
	if !app.Configured() {
		return judged{integrations.Verification{
			Status: integrations.StatusFailed,
			Note: "this deployment has no GitHub App configured, so it cannot reach " +
				"github; ask whoever operates it to configure one",
		}, "no-app"}
	}

	jwt, err := app.jwt(time.Now())
	if err != nil {
		return judged{integrations.Verification{
			Status: integrations.StatusFailed,
			Note:   "the deployment's GitHub App credential could not sign a request",
		}, "app-credential-unusable"}
	}
	found, err := client.Installation(ctx, jwt, installation)
	if err != nil {
		return judgeFailure(err, installation)
	}
	if found.Suspended {
		// A suspended installation is answered before its repositories are listed, so the
		// reach the last run established is carried rather than replaced with a zero this
		// one never checked.
		return judged{integrations.Verification{
			Status: integrations.StatusFailed,
			Note: "installation " + strconv.FormatInt(installation, 10) + " for " +
				found.Account + " is suspended in github; unsuspend it and verify again",
		}, "suspended"}
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
		return judged{integrations.Verification{
			Status: integrations.StatusVerified,
			Note: "github answered for " + found.Account + " and the installation grants " +
				"no repositories; give it at least one in the app's settings",
		}, "no-repositories"}
	}

	count := strconv.Itoa(len(granted.Repositories))
	if granted.Truncated {
		count = "at least " + count
	}
	return judged{integrations.Verification{
		Status: integrations.StatusVerified,
		Note: "verified against " + found.Account + " (" + found.AccountType + "); " +
			count + " repositories granted by " + found.RepositorySelection + " selection",
	}, "verified"}
}

// repositoryProbePage bounds how many repositories a verification lists. The point is
// proving the grant works and saying roughly how wide it is, not inventorying it.
const repositoryProbePage = 100

// judgeFailure turns what went wrong into the operator's answer. A revoked installation,
// an unknown one, a refused App credential, a rate limit and an unreachable vendor are
// five different facts, and each gets its own words.
func judgeFailure(err error, installation int64) judged {
	var refusal *APIError
	switch {
	case errors.As(err, &refusal) && refusal.Status == http.StatusNotFound:
		return gone(installation)
	case errors.As(err, &refusal) && refusal.Status == http.StatusUnauthorized:
		return judged{integrations.Verification{
			Status: integrations.StatusFailed,
			Note:   "github refused this deployment's app credential; its key or app id is wrong",
		}, "app-credential-refused"}
	case errors.As(err, &refusal):
		return judged{integrations.Verification{
			Status: integrations.StatusFailed,
			Note:   "github refused the check: " + refusal.Message,
		}, "refused"}
	case errors.Is(err, ErrRateLimited):
		return judged{integrations.Verification{
			Status: integrations.StatusFailed,
			Note:   "github is rate limiting this app; verify again in a few minutes",
		}, "rate-limited"}
	default:
		return judged{integrations.Verification{
			Status: integrations.StatusFailed,
			Note: "github could not be reached from this deployment; check its network " +
				"path and verify again",
		}, "unreachable"}
	}
}

// gone answers the 404 GitHub returns for an installation it will not serve. An
// installation this deployment verified before is one that was uninstalled or revoked —
// the operator's next step is to install the app again, not to check a number they never
// typed.
func gone(installation int64) judged {
	return judged{integrations.Verification{
		Status: integrations.StatusFailed,
		Note: "github does not know installation " + strconv.FormatInt(installation, 10) +
			" for this app; check the id, or install the app on the account again",
	}, "unknown-installation"}
}
