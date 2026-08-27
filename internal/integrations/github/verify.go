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

// The keys a GitHub verification records on the Integration. Named constants because a
// view, a test and a support answer all read the same map, and a key spelled twice is a
// fact that quietly stops being displayed.
const (
	FactAccount                = "account"
	FactAccountType            = "accountType"
	FactRepositorySelection    = "repositorySelection"
	FactRepositoryCount        = "repositoryCount"
	FactRepositoryCountAtLeast = "repositoryCountAtLeast"
	// FactManageURL is where the customer changes which repositories the installation
	// selected. Recorded rather than left to whoever displays it, because only the
	// deployment knows which GitHub it talks to.
	FactManageURL = "manageUrl"
)

// deployment is what this provider was configured with: the App credential every read is
// made under, the vendor client, and where a BROWSER reaches this deployment's GitHub.
//
// The three travel together because they are one decision — which GitHub this deployment
// talks to — and threading them apart is how the API origin and the browser origin come to
// disagree on a GitHub Enterprise host.
type deployment struct {
	app    *App
	client *Client
	// webURL is where a browser reaches GitHub, and empty where this deployment has not
	// said and cannot be guessed at. See browserOrigin.
	webURL string
}

// browserOrigin resolves where a browser reaches this deployment's GitHub, from everything
// the deployment said rather than from the installation flow alone — a deployment connected
// through the configuration form has the same question answered for it.
//
// The last case is the one that matters: an overridden API origin with no browser origin
// beside it is a GitHub Enterprise host whose web interface this build was never told
// about. It answers empty, and nothing is recorded, because a link to github.com would send
// somebody to a different company's settings page.
func browserOrigin(client *Client) string {
	switch {
	case client.reachesTheVendorsOwnAPI():
		return defaultWebURL
	default:
		return ""
	}
}

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
	ctx context.Context, where deployment, installation int64, known map[string]any,
) integrations.Verification {
	outcome := judge(ctx, where, installation, known)
	countVerification(ctx, outcome)
	return outcome.Verification
}

func judge(
	ctx context.Context, where deployment, installation int64, known map[string]any,
) judged {
	app, client := where.app, where.client
	if !app.Configured() {
		return judged{integrations.Verification{
			Status: integrations.StatusFailed,
			Note: "this deployment has no GitHub App configured, so it cannot reach " +
				"github; ask whoever operates it to configure one",
			Facts: carried(known),
		}, "no-app"}
	}

	jwt, err := app.jwt(time.Now())
	if err != nil {
		return judged{integrations.Verification{
			Status: integrations.StatusFailed,
			Note:   "the deployment's GitHub App credential could not sign a request",
			Facts:  carried(known),
		}, "app-credential-unusable"}
	}
	found, err := client.Installation(ctx, jwt, installation)
	if err != nil {
		return judgeFailure(err, installation, known)
	}
	if found.Suspended {
		// A suspended installation is answered before its repositories are listed, so the
		// reach the last run established is carried rather than replaced with a zero this
		// one never checked.
		return judged{integrations.Verification{
			Status: integrations.StatusFailed,
			Note: "installation " + strconv.FormatInt(installation, 10) + " for " +
				found.Account + " is suspended in github; unsuspend it and verify again",
			Facts: identityOf(known, found, where, installation),
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
	facts := withReach(identityOf(known, found, where, installation),
		len(granted.Repositories), granted.Truncated)
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

// identityOf records who answered, in the vocabulary an operator reads off GitHub's own
// settings page. Nothing here is a credential or an authorization: it exists so a console
// can say WHICH account is connected, and so support can answer that without asking for a
// screenshot.
func identityOf(
	known map[string]any, found Installation, where deployment, installation int64,
) map[string]any {
	facts := copied(known, 4)
	facts[FactAccount] = found.Account
	facts[FactAccountType] = found.AccountType
	facts[FactRepositorySelection] = found.RepositorySelection
	if link := manageURL(where.webURL, found, installation); link != "" {
		facts[FactManageURL] = link
	}
	return facts
}

// manageURL is where the customer changes what the installation selected: GitHub's own
// settings page, because that decision belongs where the permissions live.
//
// GitHub files an organization's installations under the organization and a personal one
// under the account, so the account type decides the shape. The login is a customer's own
// text and is escaped as a path segment; an unrecognised account type answers empty.
func manageURL(webURL string, found Installation, installation int64) string {
	if webURL == "" || found.Account == "" {
		return ""
	}
	id := strconv.FormatInt(installation, 10)
	origin := webURL
	switch found.AccountType {
	case "Organization":
		return origin + "/organizations/" + url.PathEscape(found.Account) +
			"/settings/installations/" + id
	case "User":
		return origin + "/settings/installations/" + id
	default:
		return ""
	}
}

// withReach adds how far the installation's grant went.
func withReach(facts map[string]any, repositories int, truncated bool) map[string]any {
	facts[FactRepositoryCount] = repositories
	facts[FactRepositoryCountAtLeast] = truncated
	return facts
}

// carried is what a run that established nothing records: whatever the last one did.
//
// Facts describe the INSTALLATION, not the attempt. A rate limit does not un-connect an
// account, and dropping them on every failure would cost the one thing that tells a removed
// installation apart from an id that never existed — so the second failing verification in
// a row would stop saying "no longer installed on acme-corp" and go back to "check the id".
// Nil for a record that never had any, so "nothing was ever established" stays a fact of
// its own.
func carried(known map[string]any) map[string]any {
	if len(known) == 0 {
		return nil
	}
	return copied(known, 0)
}

func copied(known map[string]any, extra int) map[string]any {
	facts := make(map[string]any, len(known)+extra)
	for key, value := range known {
		facts[key] = value
	}
	return facts
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
			Facts:  carried(known),
		}, "app-credential-refused"}
	case errors.As(err, &refusal):
		return judged{integrations.Verification{
			Status: integrations.StatusFailed,
			Note:   "github refused the check: " + refusal.Message,
			Facts:  carried(known),
		}, "refused"}
	case errors.Is(err, ErrRateLimited):
		return judged{integrations.Verification{
			Status: integrations.StatusDegraded,
			Note:   "github is rate limiting this app; verify again in a few minutes",
			Facts:  carried(known),
		}, "rate-limited"}
	default:
		return judged{integrations.Verification{
			Status: integrations.StatusFailed,
			Note: "github could not be reached from this deployment; check its network " +
				"path and verify again",
			Facts: carried(known),
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
			Facts: carried(known),
		}, "revoked"}
	}
	return judged{integrations.Verification{
		Status: integrations.StatusFailed,
		Note: "github does not know installation " + strconv.FormatInt(installation, 10) +
			" for this app; check the id, or install the app on the account again",
	}, "unknown-installation"}
}
