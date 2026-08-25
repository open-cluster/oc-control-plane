package slack

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// GrantUserToken is the recorded grant marking a user token (as against a bot's). It is
// this package's own vocabulary, not a Slack scope: classic message search works only
// under a user token, so the search tool requires this grant beside its scope.
const GrantUserToken = "user_token"

// The keys a Slack verification records on the Integration. Named constants, following
// GitHub's precedent, because a view, a test and a support answer all read the same map and
// a key spelled twice is a fact that quietly stops being displayed.
//
// FACTS ARE NOT GRANTS. Everything here is display-only: an operator with three workspaces
// reading which one this is. No authorization decision consults them and none may — what a
// token may read is Grants, which is what tool availability derives from. Nothing secret
// goes here: not the bot token, not the client secret, not the signing secret.
//
// There is deliberately no manage URL. Slack has no per-installation management page this
// service can compose without guessing a workspace domain, and a guessed one is how a
// frontend fixture came to render a button this control plane will never send.
const (
	FactWorkspace   = "workspace"
	FactWorkspaceID = "workspaceId"
	FactBotUser     = "botUser"
	FactBotUserID   = "botUserId"
)

// requiredScopes maps each OAuth scope this product ASKS FOR to what losing it costs, so a
// degraded verification names consequences an operator can act on rather than vendor
// vocabulary alone.
//
// Required means requested. A scope we deliberately do not ask for cannot be missing, and
// treating one as missing is how a correctly installed bot came to report itself broken:
// the recommended installation grants no workspace-wide search, so every recommended
// installation was degraded on the strength of a permission this product declines to hold.
var requiredScopes = map[string]string{
	"channels:read":    "listing channels",
	"channels:history": "reading channel history and threads",
	"users:read":       "resolving message authors to names",
}

// optionalScopes are grants an installation MAY hold and this product never requests. They
// are recorded when present, because a tool gated on one becomes available, and their
// absence is a stated choice rather than a gap: it does not touch the integration's status
// and it is reported as an unavailable Tool rather than a missing scope.
//
// search:read is here by decision. The default security story is that OpenCluster reasons
// over conversations it was deliberately invited into, not everything an employee can see.
var optionalScopes = map[string]string{
	"search:read": "searching messages across the workspace",
}

// probe verifies a token live against auth.test and judges what came back. It is the one
// place "verified" is decided for this provider: the far end answered as this workspace
// and this bot, with these grants — or it did not, and the note says which.
func probe(ctx context.Context, client *Client, token string) integrations.Verification {
	identity, err := client.AuthTest(ctx, token)
	if err != nil {
		return judgeFailure(err)
	}

	missing := missingScopes(identity.Scopes)
	switch {
	case len(identity.Scopes) == 0:
		// No grants recorded: with the scopes unreadable nothing can be derived, and
		// every grant-gated tool stays absent until a verification can read them. WHO
		// answered is known regardless — the two are separate facts and only one failed.
		return integrations.Verification{
			Status: integrations.StatusDegraded,
			Note: "slack accepted the token for workspace \"" + identity.Workspace +
				"\" and reported no scopes, so what the token may read could not be checked",
			Facts: factsOf(identity),
		}
	case len(missing) > 0:
		return integrations.Verification{
			Status: integrations.StatusDegraded,
			Note: "slack accepted the token for workspace \"" + identity.Workspace +
				"\" and it lacks " + strings.Join(missing, ", ") +
				", which disables " + costOf(missing),
			Grants: grantsOf(identity, token),
			Facts:  factsOf(identity),
		}
	}
	return integrations.Verification{
		Status: integrations.StatusActive,
		Note: "verified against workspace \"" + identity.Workspace + "\" as bot \"" +
			identity.Bot + "\"; every scope the tools need is granted",
		Grants: grantsOf(identity, token),
		Facts:  factsOf(identity),
	}
}

// factsOf records who answered, in the vocabulary an operator reads off Slack's own
// screens. It exists so a console can say WHICH workspace is connected as an attribute
// rather than leaving somebody to parse it out of a status sentence — and so that a client
// with nothing real to render stops inventing values.
//
// Only what the far end actually said. An empty name is omitted rather than recorded as
// empty, because "Slack did not tell us" and "the workspace is called nothing" are
// different facts and a renderer can only distinguish them by absence.
//
// Every failure branch records nothing at all, which is deliberate and is the rule GitHub
// established: facts describe the INSTALLATION, not the attempt. A refused token, a rate
// limit and an unreachable vendor establish nothing about a workspace, and the column keeps
// what the last verification that reached one put there.
func factsOf(identity Identity) map[string]any {
	facts := make(map[string]any, 4)
	for key, value := range map[string]string{
		FactWorkspace:   identity.Workspace,
		FactWorkspaceID: identity.WorkspaceID,
		FactBotUser:     identity.Bot,
		FactBotUserID:   identity.BotUserID,
	} {
		if value != "" {
			facts[key] = value
		}
	}
	if len(facts) == 0 {
		return nil
	}
	return facts
}

// grantsOf records the verified reality tool availability derives from: the granted
// scopes verbatim, plus the token's kind — classic message search is user-token-only,
// so "is this a user token" is a fact worth recording, read from the token's own
// documented prefixes (xoxp for a plain user token, xoxe.xoxp for a rotated one) at the
// one moment the plaintext is in hand.
func grantsOf(identity Identity, token string) []string {
	grants := append([]string(nil), identity.Scopes...)
	if strings.HasPrefix(token, "xoxp-") || strings.HasPrefix(token, "xoxe.xoxp-") {
		grants = append(grants, GrantUserToken)
	}
	return grants
}

// judgeFailure turns what went wrong into the operator's answer. Three different facts get
// three different judgements: a refused credential is failed, a rate limit is degraded —
// nothing about the credential is known to be wrong — and an unreachable vendor is failed
// with that said plainly.
func judgeFailure(err error) integrations.Verification {
	var refusal *APIError
	switch {
	case errors.As(err, &refusal):
		return integrations.Verification{
			Status: integrations.StatusFailed,
			Note: "slack refused the token (" + refusal.Code + "); paste a current bot " +
				"token and verify again",
		}
	case errors.Is(err, ErrRateLimited):
		return integrations.Verification{
			Status: integrations.StatusDegraded,
			Note:   "slack is rate limiting this workspace's token; verify again in a minute",
		}
	default:
		return integrations.Verification{
			Status: integrations.StatusFailed,
			Note:   "slack could not be reached from this deployment; check its network path and verify again",
		}
	}
}

// missingScopes reports which needed scopes the token was not granted, in a stable order.
func missingScopes(granted []string) []string {
	held := make(map[string]bool, len(granted))
	for _, scope := range granted {
		held[scope] = true
	}
	var missing []string
	for scope := range requiredScopes {
		if !held[scope] {
			missing = append(missing, scope)
		}
	}
	sort.Strings(missing)
	return missing
}

// costOf says what the missing scopes stop working, in the operator's language.
func costOf(missing []string) string {
	costs := make([]string, 0, len(missing))
	for _, scope := range missing {
		costs = append(costs, requiredScopes[scope])
	}
	return strings.Join(costs, " and ")
}
