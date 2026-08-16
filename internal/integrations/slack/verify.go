package slack

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// scopeGrants maps each OAuth scope the tools need to what losing it costs, so a degraded
// verification names consequences an operator can act on rather than vendor vocabulary
// alone.
var scopeGrants = map[string]string{
	"channels:read":    "listing channels",
	"channels:history": "reading channel history and threads",
	"search:read":      "searching messages",
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
		return integrations.Verification{
			Status: integrations.StatusDegraded,
			Note: "slack accepted the token for workspace \"" + identity.Workspace +
				"\" and reported no scopes, so what the token may read could not be checked",
		}
	case len(missing) > 0:
		return integrations.Verification{
			Status: integrations.StatusDegraded,
			Note: "slack accepted the token for workspace \"" + identity.Workspace +
				"\" and it lacks " + strings.Join(missing, ", ") +
				", which disables " + costOf(missing),
		}
	}
	return integrations.Verification{
		Status: integrations.StatusActive,
		Note: "verified against workspace \"" + identity.Workspace + "\" as bot \"" +
			identity.Bot + "\"; every scope the tools need is granted",
	}
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
	for scope := range scopeGrants {
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
		costs = append(costs, scopeGrants[scope])
	}
	return strings.Join(costs, " and ")
}
