package slack

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

const grantUserToken = "user_token"

var requiredScopes = map[string]string{
	"channels:read":    "listing channels",
	"channels:history": "reading channel history and threads",
	"users:read":       "resolving message authors to names",
}

var optionalScopes = map[string]string{
	"search:read": "searching messages across the workspace",
}

func probe(ctx context.Context, client *Client, token string) integrations.Verification {
	identity, err := client.AuthTest(ctx, token)
	if err != nil {
		return judgeFailure(err)
	}

	missing := missingScopes(identity.Scopes)
	switch {
	case len(identity.Scopes) == 0:
		return integrations.Verification{
			Status: integrations.StatusVerified,
			Note: "slack accepted the token for workspace \"" + identity.Workspace +
				"\" and reported no scopes, so what the token may read could not be checked",
		}
	case len(missing) > 0:
		return integrations.Verification{
			Status: integrations.StatusVerified,
			Note: "slack accepted the token for workspace \"" + identity.Workspace +
				"\" and it lacks " + strings.Join(missing, ", ") +
				", which disables " + costOf(missing),
			Grants: grantsOf(identity, token),
		}
	}
	return integrations.Verification{
		Status: integrations.StatusVerified,
		Note: "verified against workspace \"" + identity.Workspace + "\" as bot \"" +
			identity.Bot + "\"; every scope the tools need is granted",
		Grants: grantsOf(identity, token),
	}
}

func grantsOf(identity Identity, token string) []string {
	grants := append([]string(nil), identity.Scopes...)
	if strings.HasPrefix(token, "xoxp-") || strings.HasPrefix(token, "xoxe.xoxp-") {
		grants = append(grants, grantUserToken)
	}
	return grants
}

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
			Status: integrations.StatusFailed,
			Note:   "slack is rate limiting this workspace's token; verify again in a minute",
		}
	default:
		return integrations.Verification{
			Status: integrations.StatusFailed,
			Note:   "slack could not be reached from this deployment; check its network path and verify again",
		}
	}
}

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

func costOf(missing []string) string {
	costs := make([]string, 0, len(missing))
	for _, scope := range missing {
		costs = append(costs, requiredScopes[scope])
	}
	return strings.Join(costs, " and ")
}
