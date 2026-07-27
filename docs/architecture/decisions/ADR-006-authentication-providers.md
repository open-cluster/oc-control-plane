# Authentication providers vary; tenancy and authorization do not

Clerk remains the identity provider for the hosted product and direct OIDC is added for
self-hosted and air-gapped deployments, but neither is ever visible past the authentication
boundary. Every scheme maps onto one canonical principal — tenant id, user id, roles,
scopes — and there is exactly one authorization system, which knows nothing about how the
caller authenticated. Provider-specific types may not appear in any domain module.

This is largely a ratification of what the code already does rather than a new direction,
which is the reason it is cheap. Verified 2026-07-26: `OpenCluster.Auth` carries no Clerk
SDK dependency at all — the only authentication package is
`Microsoft.AspNetCore.Authentication.JwtBearer`, and Clerk is consumed as an ordinary JWT
issuer whose claim names are configuration. `OpenClusterClaimTypes` already defines the
canonical claims, `ITenantContext` already exposes the canonical principal, and
`ClerkClaimsMapper` is already the adapter between them. The design is already proven
against a second provider: API keys authenticate through an entirely separate handler and
produce the same canonical claims for the same authorization policies.

## The real gap is provisioning, not authentication

Adding an OIDC issuer is a sibling claims mapper and configuration. What does not exist is
a way to create tenants, users, and memberships without Clerk: `ClerkWebhookProcessor` is
the only writer of all three repositories, so a self-hosted deployment with no Clerk
webhook source has no path to a populated tenancy model. Self-hosted therefore needs a
provisioning seam — just-in-time provisioning from a verified token, SCIM, or
administrator-managed membership — and that, not the authentication scheme, is the work.

## Considered and rejected: OIDC only, dropping Clerk

Rejected because the stated precondition is not met. OIDC-only is the right answer only if
the immediate market is exclusively enterprise and self-hosted, and the accepted tier model
sells hosted Starter and Business tiers. Choosing it would mean building user lifecycle,
organization creation, invitations, email verification, session revocation, and MFA — all
undifferentiated work competing directly with the R1 and Stage 1C bandwidth that carries
the product's actual value.

The rejection is also low-stakes in a way worth recording: because there is no Clerk SDK,
the switching cost is a claims mapper and a provisioning path. Deferring this decision does
not compound, so it should be revisited on evidence — a self-hosted or air-gapped deal —
rather than on schedule.

## Consequences

- One authorization system. Adding a provider adds a claims mapper and, where the provider
  cannot push memberships, a provisioning path. It never adds a policy engine.
- A provider-specific type outside the authentication boundary is a defect. One exists
  today: Svix webhook verification lives under `OpenCluster.Auth.Clerk` but is consumed by
  the Resend delivery path in the alerting module. Svix is a signing scheme shared by two
  unrelated vendors and belongs in a provider-neutral namespace.
- Tenancy data stays local and authoritative for authorization. An external provider may be
  the system of record for identity, never for OpenCluster's authorization decisions.
- The hosted and self-hosted deployments must not diverge into two authorization behaviours;
  the same policy tests run against both authentication schemes.
