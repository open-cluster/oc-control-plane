package datadog

import (
	"context"
	"errors"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// probe verifies the sealed credential live, by making the same call the tools make: a
// bounded monitors read. Datadog has no dedicated identity endpoint that checks an
// application key, so "verified" here means what it means everywhere else in this
// codebase — the far end answered the read the tools actually perform.
func probe(ctx context.Context, client *Client, site, credentialJSON string) integrations.Verification {
	cred, err := parseCredential(credentialJSON)
	if err != nil {
		return integrations.Verification{
			Status: integrations.StatusFailed,
			Note:   "the stored credential is not readable; paste the API key and application key again",
		}
	}

	if _, err := client.Monitors(ctx, site, cred, MonitorsQuery{Limit: 1}); err != nil {
		return judgeFailure(err)
	}
	return integrations.Verification{
		Status: integrations.StatusActive,
		Note:   "verified against Datadog site \"" + site + "\": the api key and application key both read",
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
			Note: "datadog refused the keys (" + refusal.Detail + "); a read needs BOTH a " +
				"valid api key and a valid application key — paste a current pair and verify again",
		}
	case errors.As(err, &refusal):
		return integrations.Verification{
			Status: integrations.StatusFailed,
			Note:   "datadog refused the call (" + refusal.Detail + "); verify the keys and site and try again",
		}
	case errors.Is(err, ErrRateLimited):
		return integrations.Verification{
			Status: integrations.StatusDegraded,
			Note:   "datadog is rate limiting this account's keys; verify again shortly",
		}
	default:
		return integrations.Verification{
			Status: integrations.StatusFailed,
			Note:   "datadog could not be reached from this deployment; check its network path and verify again",
		}
	}
}
