package sentry

import (
	"context"
	"errors"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// probe verifies a token live against the organization endpoint and judges what came
// back. It is the one place "verified" is decided for this provider: the far end answered
// as this organization — or it did not, and the note says which.
func probe(ctx context.Context, client *Client, token, orgSlug string) integrations.Verification {
	organization, err := client.Organization(ctx, token, orgSlug)
	if err != nil {
		return judgeFailure(err)
	}
	return integrations.Verification{
		Status: integrations.StatusActive,
		Note:   "verified against organization \"" + organization.Name + "\"",
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
			Note:   "sentry refused the token (" + refusal.Detail + "); paste a current auth token and verify again",
		}
	case errors.As(err, &refusal) && refusal.Status == 404:
		return integrations.Verification{
			Status: integrations.StatusFailed,
			Note:   "sentry found no organization with the configured slug; check organizationSlug and verify again",
		}
	case errors.As(err, &refusal):
		return integrations.Verification{
			Status: integrations.StatusFailed,
			Note:   "sentry refused the call (" + refusal.Detail + "); verify the token and organization and try again",
		}
	case errors.Is(err, ErrRateLimited):
		return integrations.Verification{
			Status: integrations.StatusDegraded,
			Note:   "sentry is rate limiting this organization's token; verify again shortly",
		}
	default:
		return integrations.Verification{
			Status: integrations.StatusFailed,
			Note:   "sentry could not be reached from this deployment; check its network path and verify again",
		}
	}
}
