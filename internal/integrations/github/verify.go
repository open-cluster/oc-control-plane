package github

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// The keys a GitHub verification records on the Integration. Named constants because a
// view, a test and a support answer all read the same map, and a key spelled twice is a
// fact that quietly stops being displayed.
const (
	FactAccount                = "account"
	FactAccountType            = "accountType"
	FactRepositorySelection    = "repositorySelection"
	FactRepositoryCount        = "repositoryCount"
	FactRepositoryCountAtLeast = "repositoryCountAtLeast"
)

// judged is one verification plus the closed word the counter attributes it with. The
// reason is this build's own vocabulary and never anything GitHub sent: a value an
// outsider could choose would mint an unbounded number of time series.
type judged struct {
	integrations.Verification
	reason string
}

// probe verifies one installation live: GitHub is asked for the installation under the
// App's own credential, a token is minted under it, and the selected repositories are
// listed. It is the one place "verified" is decided for this provider — the far end
// answered as this account with these repositories, or it did not, and the note says
// which. Every run is counted, so a deployment whose App registration is broken is a
// number rather than a support ticket.
//
// known is what a previous verification established about this same installation, and it
// is what separates an installation that was removed from one that never existed: GitHub
// answers 404 for both, and only the record can tell them apart.
func probe(
	ctx context.Context, app *App, client *Client, installation int64, known map[string]any,
) integrations.Verification {
	outcome := judge(ctx, app, client, installation, known)
	countVerification(ctx, outcome)
	return outcome.Verification
}

func judge(
	ctx context.Context, app *App, client *Client, installation int64, known map[string]any,
) judged {
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
		return judgeFailure(err, installation, known)
	}
	if found.Suspended {
		return judged{integrations.Verification{
			Status: integrations.StatusFailed,
			Note: "installation " + strconv.FormatInt(installation, 10) + " for " +
				found.Account + " is suspended in github; unsuspend it and verify again",
			Facts: factsOf(found, 0, false),
		}, "suspended"}
	}

	token, err := app.installationToken(ctx, installation)
	if err != nil {
		return judgeFailure(err, installation, known)
	}
	granted, err := client.Repositories(ctx, token, repositoryProbePage, 1)
	if err != nil {
		return judgeFailure(err, installation, known)
	}
	facts := factsOf(found, len(granted.Repositories), granted.Truncated)
	if len(granted.Repositories) == 0 {
		return judged{integrations.Verification{
			Status: integrations.StatusDegraded,
			Note: "github answered for " + found.Account + " and the installation grants " +
				"no repositories; give it at least one in the app's settings",
			Facts: facts,
		}, "no-repositories"}
	}

	count := strconv.Itoa(len(granted.Repositories))
	if granted.Truncated {
		count = "at least " + count
	}
	return judged{integrations.Verification{
		Status: integrations.StatusActive,
		Note: "verified against " + found.Account + " (" + found.AccountType + "); " +
			count + " repositories granted by " + found.RepositorySelection + " selection",
		Facts: facts,
	}, "verified"}
}

// factsOf records what this run established about the installation, in the vocabulary an
// operator reads off GitHub's own settings page. Nothing here is a credential or an
// authorization: it exists so a console can say WHICH account is connected and how far it
// reaches, and so support can answer that without asking for a screenshot.
func factsOf(found Installation, repositories int, truncated bool) map[string]any {
	return map[string]any{
		FactAccount:                found.Account,
		FactAccountType:            found.AccountType,
		FactRepositorySelection:    found.RepositorySelection,
		FactRepositoryCount:        repositories,
		FactRepositoryCountAtLeast: truncated,
	}
}

// repositoryProbePage bounds how many repositories a verification lists. The point is
// proving the grant works and saying roughly how wide it is, not inventorying it.
const repositoryProbePage = 100

// judgeFailure turns what went wrong into the operator's answer. A revoked installation,
// an unknown one, a refused App credential, a rate limit and an unreachable vendor are
// five different facts, and each gets its own words.
func judgeFailure(err error, installation int64, known map[string]any) judged {
	var refusal *APIError
	switch {
	case errors.As(err, &refusal) && refusal.Status == http.StatusNotFound:
		return gone(installation, known)
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
			Status: integrations.StatusDegraded,
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
func gone(installation int64, known map[string]any) judged {
	if account, _ := known[FactAccount].(string); account != "" {
		return judged{integrations.Verification{
			Status: integrations.StatusFailed,
			Note: "the github app is no longer installed on " + account +
				"; it was uninstalled or its access was revoked, so connect github again",
		}, "revoked"}
	}
	return judged{integrations.Verification{
		Status: integrations.StatusFailed,
		Note: "github does not know installation " + strconv.FormatInt(installation, 10) +
			" for this app; check the id, or install the app on the account again",
	}, "unknown-installation"}
}
