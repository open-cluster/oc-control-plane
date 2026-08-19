package pagerduty

import (
	"context"
	"errors"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// probe verifies the token live, by making the same call the tools make: a bounded
// incidents read. PagerDuty has no dedicated identity endpoint this provider needs, so
// "verified" here means what it means everywhere else in this codebase — the far end
// answered the read the tools actually perform.
func probe(ctx context.Context, client *Client, token string) integrations.Verification {
	if _, err := client.Incidents(ctx, token, IncidentsQuery{Limit: 1}); err != nil {
		return judgeFailure(err)
	}
	return integrations.Verification{
		Status: integrations.StatusActive,
		Note:   "verified against PagerDuty: the token reads incidents",
	}
}

// judgeFailure turns what went wrong into the operator's answer. A refused token is
// failed, a rate limit is degraded — nothing about the token is known to be wrong — and
// an unreachable vendor is failed with that said plainly.
func judgeFailure(err error) integrations.Verification {
	var refusal *APIError
	switch {
	case errors.As(err, &refusal) && (refusal.Status == 401 || refusal.Status == 403):
		return integrations.Verification{
			Status: integrations.StatusFailed,
			Note:   "pagerduty refused the token (" + refusal.Message + "); paste a current API key and verify again",
		}
	case errors.As(err, &refusal):
		return integrations.Verification{
			Status: integrations.StatusFailed,
			Note:   "pagerduty refused the call (" + refusal.Message + "); verify the token and try again",
		}
	case errors.Is(err, ErrRateLimited):
		return integrations.Verification{
			Status: integrations.StatusDegraded,
			Note:   "pagerduty is rate limiting this account's token; verify again shortly",
		}
	default:
		return integrations.Verification{
			Status: integrations.StatusFailed,
			Note:   "pagerduty could not be reached from this deployment; check its network path and verify again",
		}
	}
}
