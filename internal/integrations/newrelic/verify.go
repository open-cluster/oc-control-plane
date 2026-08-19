package newrelic

import (
	"context"
	"errors"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// probe verifies the user key live, by making the same call the tools make: a bounded
// issues read. NerdGraph has no dedicated identity endpoint, so "verified" here means what
// it means everywhere else in this codebase — the far end answered the read the tools
// actually perform.
func probe(ctx context.Context, client *Client, region string, accountID int, key string) integrations.Verification {
	if _, err := client.Issues(ctx, region, key, accountID, IssuesQuery{Limit: 1}); err != nil {
		return judgeFailure(err)
	}
	return integrations.Verification{
		Status: integrations.StatusActive,
		Note:   "verified against New Relic account in the \"" + region + "\" region",
	}
}

// judgeFailure turns what went wrong into the operator's answer. A refused credential is
// failed, a rate limit is degraded — nothing about the credential is known to be wrong —
// and an unreachable vendor is failed with that said plainly.
func judgeFailure(err error) integrations.Verification {
	var refusal *APIError
	switch {
	case errors.As(err, &refusal) && (refusal.Status == 401 || refusal.Status == 403):
		return integrations.Verification{
			Status: integrations.StatusFailed,
			Note:   "new relic refused the user key (" + refusal.Detail + "); paste a current key and verify again",
		}
	case errors.As(err, &refusal):
		return integrations.Verification{
			Status: integrations.StatusFailed,
			Note:   "new relic refused the call (" + refusal.Detail + "); verify the key, account id and region and try again",
		}
	case errors.Is(err, ErrRateLimited):
		return integrations.Verification{
			Status: integrations.StatusDegraded,
			Note:   "new relic is rate limiting this account's key; verify again shortly",
		}
	default:
		return integrations.Verification{
			Status: integrations.StatusFailed,
			Note:   "new relic could not be reached from this deployment; check its network path and verify again",
		}
	}
}
