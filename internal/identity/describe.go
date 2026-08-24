package identity

import (
	"net/http"

	"github.com/open-cluster/oc-control-plane/internal/describe"
)

// Describe is this capability's contribution to the deployment's self-description.
//
// BODIES ONLY. Every write here refuses a field it does not declare, and until now the only
// way to learn the shape was to send something and read a 400 — so the schemas are what this
// contributes.
//
// It contributes no listings, and that absence is a fact rather than an oversight worth
// hiding: this capability's listings page with `limit` and `after` of their own rather than
// through internal/table, so they have no Spec to publish. Bringing them onto the shared
// listing contract is a wire-visible change and belongs to its own decision, not to the
// document that would describe it.
func (h Handlers) Describe() describe.Contribution {
	body := func(method, pattern string, example any) describe.Body {
		return describe.Body{Route: method + " " + organizationBase + pattern, Example: example}
	}
	return describe.Contribution{
		Bodies: []describe.Body{
			body(http.MethodPost, "/bootstrap", localCredentialRequest{}),
			body(http.MethodPost, "/sign-in/local", localCredentialRequest{}),
			body(http.MethodPost, "/members", localMemberRequest{}),
			body(http.MethodPost, "/members/oidc", oidcMemberRequest{}),
			body(http.MethodPut, "/members/{user}", memberRequest{}),
			body(http.MethodPut, "/members/{user}/password", localPasswordRequest{}),
			body(http.MethodPut, "/policy", policyRequest{}),
			body(http.MethodPost, "/service-accounts", serviceAccountRequest{}),
			body(http.MethodPost, "/api-tokens", tokenRequest{}),
		},
	}
}
