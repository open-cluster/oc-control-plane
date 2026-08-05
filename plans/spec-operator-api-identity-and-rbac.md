# Spec — Operator API: identity, session, tenancy enforcement, RBAC and audit

Status: BUILT 2026-08-05, with migrations 0011 and 0012. **Stories 1–16, 18–31 and 33 are
implemented and asserted through the HTTP boundary.** Three are not — 17, 32 and 34 — each for a
stated reason rather than by omission. **Story 21 was half done and is now whole: the retention
pruner was built on 2026-08-05.** See "What was built, and what was not" at the foot of this
document.

SAML 2.0 (story 12) and SCIM (stories 13 and 14) were deferred in the first pass and built in
the second, in migration 0012.

Repo: `oc-control-plane`. Prerequisite for every frontend gate.
Audit basis: `oc-frontend/plans/audit-2026-08-04-enterprise-forensic.md` §1 B1–B4.

## Problem Statement

An operator cannot sign in to OpenCluster. There is no session, no identity, and no way for the
browser to authenticate at all: the frontend sends a cookie session
(`oc-frontend/shared/lib/transport.ts`, `credentials: 'include'`), and the control plane requires a
single shared static bearer token (`internal/operator/operator.go:97-111`). Every browser request
to a real control plane answers `401`.

Behind that, the operator surface is cross-tenant by design — its own documentation says so
(`internal/operator/operator.go:14-15`) — so whoever holds the one token can read and mutate any
Organization by editing a URL path segment. There are no roles. There is no record of who did
anything: `callerOf` returns `request.RemoteAddr`, and the code states outright that it "can say
where the claim came from and never who made it" (`:234-236`).

The consequences for the people involved:

- An **SRE** cannot be given access to the product without being given access to every tenant.
- A **security reviewer** at a design partner cannot be answered on authentication, authorization,
  session lifetime, or audit — the honest answer to all four is "none".
- A **platform administrator** cannot revoke a departing colleague's access, because there is
  nothing individual to revoke.
- An **auditor** cannot answer "who disabled this Connection and when".

## Solution

Give the control plane a real principal, and make every existing handler ask about it.

An operator signs in through an OIDC Authorization Code flow with PKCE against an identity
provider the Organization configures. The control plane exchanges the code, resolves or
just-in-time-provisions a User, resolves that User's memberships, and issues its own opaque
server-side session as an `HttpOnly`, `Secure`, `SameSite=Lax` cookie. `GET /operator/v1/session`
answers who is signed in and which Organizations they may read. `POST /operator/v1/session/sign-out`
deletes the server-side session row, so the credential is dead before the response is written.

Every operator handler resolves its Organization from the path **and then verifies the principal
holds a membership in it**. A request naming an Organization the principal is not a member of
answers `404`, not `403` — a `403` confirms the tenant exists.

Every handler declares the permission it requires. The check runs in one middleware, from a table,
so a new route without a declared permission fails to compile rather than defaulting to open.

Every state change and every cross-tenant read writes an `AuditEvent` row inside the same
transaction as the change it records. An audit write that fails rolls back the operation it
describes.

The existing shared static token survives as a **service-account credential** — but scoped: bound
to one Organization, one role, an expiry, and a revocation state, rather than being ambient
root.

## User Stories

1. As an SRE, I want to sign in with my company identity provider, so that I do not have to be
   handed a shared secret over Slack.
2. As an SRE, I want my session to survive a page reload, so that a browser refresh during an
   incident does not cost me my place.
3. As an SRE, I want to see which Organization and which principal the interface is reading as, so
   that I never act on the wrong tenant's data.
4. As an SRE, I want Sign out to end my session on the server, so that a shared or stolen laptop
   does not carry a live credential.
5. As an SRE, I want a session that has expired to return me to sign-in with an explanation, so
   that I do not read a screen of error states and assume the product is broken.
6. As a platform administrator, I want to configure an OIDC identity provider for my Organization,
   so that access follows our existing joiner/mover/leaver process.
7. As a platform administrator, I want to configure more than one identity provider, so that a
   merged or contractor population can be served without a second tenant.
8. As a platform administrator, I want to restrict just-in-time provisioning to verified email
   domains, so that an unrelated account at the same IdP cannot enter my Organization.
9. As a platform administrator, I want to map identity-provider groups to OpenCluster roles, so
   that role assignment is not a second directory I have to maintain by hand.
10. As a platform administrator, I want to revoke another user's active sessions immediately, so
    that offboarding takes effect before the next token refresh.
11. As a platform administrator, I want to set the session lifetime for my Organization, so that
    it matches our security policy rather than a vendor default.
12. As a platform administrator, I want SAML 2.0 available where our IdP does not offer OIDC, so
    that identity is not the reason we cannot deploy.
13. As a platform administrator, I want SCIM provisioning of users and groups, so that our
    directory stays the source of truth.
14. As a platform administrator, I want a user removed in SCIM to lose access without a manual
    step, so that deprovisioning is not a checklist item somebody forgets.
15. As an integration manager, I want permission to create and validate Connections without
    permission to change identity settings, so that my access matches my job.
16. As an investigator, I want to open and cancel Investigations but not delete Connections, so
    that an incident-time mistake cannot damage the estate.
17. As a responder, I want to record that I performed a remediation, so that the recovery
    verification has a stated human origin.
18. As a viewer, I want read-only access to Investigations, so that I can be looped in during an
    incident without being given the ability to change anything.
19. As an auditor, I want read access to the audit log and nothing else, so that my access does not
    itself become a risk.
20. As an auditor, I want every audit event to name the actor, the Organization, the target, the
    time, the source address and the outcome, so that I can answer a security questionnaire from
    the record.
21. As an auditor, I want audit events to be immutable and retained on a stated schedule, so that
    the record is admissible in a review.
22. As an auditor, I want failed authorization attempts recorded, so that credential probing is
    visible.
23. As an auditor, I want identity-configuration changes recorded with the previous and new value,
    so that a weakened policy is discoverable.
24. As a security reviewer, I want a request naming an Organization I do not belong to to be
    indistinguishable from one naming an Organization that does not exist, so that the API cannot
    be used to enumerate tenants.
25. As a security reviewer, I want every mutation authorized server-side regardless of what the
    interface offered, so that hiding a button is never the control.
26. As a security reviewer, I want the Environment scope enforced on the API and not only in the
    path, so that a caller cannot read another Environment's Connections by editing a URL.
27. As a platform administrator, I want to create a service account with a scoped API token, so
    that automation runs as something other than a person.
28. As a platform administrator, I want each API token bound to one Organization, one role and an
    expiry, so that a leaked token has a blast radius and a deadline.
29. As a platform administrator, I want to see when each API token was last used, so that I can
    retire the ones nobody needs.
30. As a platform administrator, I want to revoke an API token immediately, so that a leak is a
    five-second fix.
31. As a platform administrator, I want a token shown exactly once at creation, so that the system
    never holds a readable copy.
32. As an on-call engineer, I want break-glass access that is loudly audited and time-boxed, so
    that an emergency does not require sharing a permanent credential.
33. As a developer, I want a new route without a declared permission to fail the build, so that
    "forgot to add the check" cannot ship.
34. As a developer, I want the frontend's declared contract asserted against the running control
    plane in CI, so that a divergence like the present one is caught the day it is introduced.

## Implementation Decisions

**New packages.** `internal/identity` (principal, membership, IdP configuration, OIDC and SAML
flows, SCIM), `internal/session` (server-side session records and their lifecycle), `internal/authz`
(permission table and the middleware that reads it), `internal/audit` (append-only event store and
its read API). `internal/operator` keeps the listener and delegates the credential decision to
`internal/authz`.

Note `opencluster-relay` already has `internal/identity`, `internal/session` and `internal/audit`
packages for the relay's own enrolment and workload identity. Those are a different subject — a
relay's identity, not a person's — and must not be merged. Naming collides across repos; the
concepts do not.

**Schema.** New tables, all Organization-scoped except `users` and `sessions`, which reference it:
`users`, `organization_memberships` (user, organization, role, source: `manual` | `jit` | `scim`),
`identity_providers` (organization, protocol, issuer, client id, encrypted client secret, verified
domains, JIT policy, group-to-role map), `sessions` (opaque id, user, organization, issued,
expires, last seen, revoked, user agent, address), `service_accounts`, `api_tokens` (digest only,
organization, role, expiry, last used, revoked), `audit_events`.

`audit_events` is append-only and enforced as such at the database level, not by convention. It
carries: organization, actor kind (`user` | `service_account` | `system`), actor id, actor display
name at time of writing, action, target kind, target id, outcome, source address, request id,
occurred-at, and a JSONB detail column that never contains a credential or evidence content.

**Roles.** Seven, as the brief proposes, each a fixed set of permissions rather than an editable
one in the first release: Organization owner, Platform administrator, Investigator, Responder,
Viewer, Auditor, Integration manager. Custom roles are out of scope. A permission is a verb-noun
string (`connection.create`, `relay.token.rotate`, `audit.read`).

**The permission table is the API surface's index.** Every route registers as
`(method, pattern, permission)`. `Router()` builds the mux from that table; a route registered
without a permission is a compile error, and a gate test in `internal/gates/` asserts the table
covers every registered pattern. This is the mechanism that makes story 33 true.

**Tenancy enforcement.** `tenancy.Organization` already exists and is well-shaped
(`internal/tenancy/tenancy.go`) — unexported field, constructor validation, so a store function can
trust what it receives. Extend the pattern: introduce `authz.Principal` with the same discipline,
and change every store call site to take `(ctx, principal, organization, …)`. A membership miss
answers `404`.

**Environment enforcement.** `internal/connection/handlers.go` currently scopes by the
`{environment}` path segment. That stays, and gains a check that the principal's role permits
reading that Environment. Environment-level role scoping (a Viewer in staging only) is **deferred**
and recorded as such — the roles ship Organization-wide first, and the schema leaves room by
putting the membership row, not the user row, in the scope position.

**Path corrections, breaking, versioned together.** The frontend's contract is the better-designed
of the two and the control plane moves to it, not the reverse:

- `GET /operator/v1/organizations/{org}/integrations` — organization-scoped, so
  `configuredConnections` can be counted per tenant.
- `GET|POST /operator/v1/organizations/{org}/connections` — Organization-wide list, with
  `environmentId` as a filter rather than a path segment. Creation still derives the Environment
  server-side from the request body's Connection, per ADR-003.
- `POST …/connections/{id}/enabled` replaces the `/enable` and `/disable` pair — one operation with
  a body, so "set to the state I want" is idempotent.
- `…/connections/{id}/trigger/rotate-secret` gains the `trigger/` segment, because rotating a
  trigger verification secret and rotating an evidence credential are different operations and the
  current path claims to be both.

**Session transport.** Opaque server-side session id in a cookie: `HttpOnly`, `Secure`,
`SameSite=Lax`, `Path=/`, no JWT. A JWT would make story 4 and story 10 unimplementable without a
revocation list, which is a session table with extra steps. `SameSite=Lax` plus an
`Origin`-checking middleware on every unsafe method covers CSRF; no separate CSRF token.

**Sign-out.** Deletes the session row and clears the cookie in the same response. Returns
`{ signedOut: true }`, which is then a true statement rather than the present one.

**Contract shapes.** `Principal.roles` and `Principal.scopes` in
`oc-frontend/entities/contract/session.ts` are already the right shape and gain a producer.
`OrganizationMembership.role` is already correctly on the membership rather than the principal.
The frontend contract does not change for session; the control plane implements it.

**What is deliberately not built in this spec.** Custom roles. Environment-scoped role assignment.
Nested groups. IdP-initiated SAML. Attribute-based access control. Each is recorded as deferred
with the reason, not silently absent.

## Testing Decisions

A good test here asserts what a caller observes across the HTTP boundary: a status code, a
`Set-Cookie`, a body shape, a row in `audit_events`. It does not assert that a particular function
was called. The existing suite's style is right and the new work follows it — `internal/gates/`
already holds boundary and enum tests that fail the build on a structural mistake, and that is the
prior art for the permission-table gate.

**Tenancy boundary tests.** `internal/storage/boundary_test.go` is the model: extend it so every
new store function is asserted to refuse a principal without a membership. A table test over the
route table, not one test per route, so a new route is covered by existing code.

**Session lifecycle tests.** Sign in issues a row and a cookie; the cookie authenticates; sign-out
deletes the row; the same cookie afterwards answers `401`; an expired row answers `401`;
administrator revocation of another user's session takes effect on the next request.

**Authorization tests.** For each of the seven roles, a table of (route, expected status). The
table is the specification of the role, and reading it is how a reviewer answers "what can an
Investigator do".

**Enumeration test.** A member of org A requesting org B receives byte-identical responses for
"exists but not a member" and "does not exist".

**Audit tests.** Every mutating route writes exactly one event naming the actor; a forced audit
write failure rolls back the operation; the events table refuses an update and a delete.

**OIDC tests.** Against a local mock issuer, not a live provider: PKCE verifier mismatch is
refused; a replayed authorization code is refused; `state` mismatch is refused; an unverified email
domain is refused where the policy requires verification; group-to-role mapping produces the
expected membership.

**Contract-drift, made non-skippable.** `oc-frontend/tests/e2e/contract-drift.spec.ts` currently
calls `requireControlPlane()` at file scope and skips the suite when nothing is listening — which
is why the divergence in the audit went unnoticed for as long as it did. CI must run it against a
real `cmd/controlplane` with a seeded session, and a skip in CI must fail the job. The spec also
needs a credential: it presently sends none.

## Out of Scope

Custom role definitions. Environment-scoped role assignment. Nested IdP groups. IdP-initiated
SAML. Attribute-based access control. Multi-factor authentication implemented by OpenCluster
itself — MFA is the identity provider's job and delegating it is the correct answer, stated rather
than omitted. Anything on the frontend, which is `oc-frontend/plans/spec-frontend-production-console.md`.

## Further Notes

The old frontend at `D:\Development\opencluster-web` uses Clerk (`@clerk/nextjs` in its
`package.json`). That is a live precedent and worth a deliberate decision rather than a default:
Clerk would deliver stories 1–5 in days and stories 6–14 as a paid tier, at the cost of a
third-party in the authentication path — which is the first thing a design partner's security
review will ask about. The `Principal` contract is provider-neutral by construction
(`oc-frontend/entities/contract/session.ts`), so the adapter boundary already exists and the
decision is reversible. **Recommendation: build the session and membership tables in the control
plane, and treat the IdP as pluggable — Clerk or a direct OIDC client behind the same
`internal/identity` interface.** Do not put the tenancy or authorization decision in a vendor.

`internal/operator/operator.go` also serves two routes with no frontend consumer:
`session-conflicts` and `clear-conflict`, which surface relay-credential-theft detection. Once a
principal exists, `clear-conflict` becomes one of the highest-privilege operations in the product —
it destroys a security finding — and must require Platform administrator and write an audit event
at warning level. It already logs loudly (`:237-240`); it needs the record, the permission and a
surface.

## What was built, and what was not

Written 2026-08-05, after the work. It corrects the document rather than being appended to it as
a second opinion: where the implementation departed from what is written above, the departure is
here with its reason, and the paragraph it departs from stands as the intent.

**Built and asserted across the HTTP boundary.** OIDC Authorization Code with PKCE against a
provider a tenant configures, with the identity token verified against the issuer's own published
keys rather than trusted because the channel was TLS. Opaque server-side sessions in an
`HttpOnly`, `Secure`, `SameSite=Lax`, `__Host-` cookie, digest-only at rest, deleted on sign-out.
`GET /operator/v1/session` and `POST /operator/v1/session/sign-out`. Seven fixed roles as a
compiled table. Every route registered as `(method, pattern, permission)` with the mux built from
that table. A membership miss answering 404 byte-identically to an Organization that does not
exist. Service accounts and API tokens bound to one Organization, one role and an expiry, shown
once, revocable, with a last-used stamp. An append-only `audit_events` table the database itself
refuses to update, delete or truncate. Every state change recording itself in the transaction that
made it. The four path corrections. The permission-table gate.

**Not built, each for a reason.**

- **Break-glass access (story 32).** Absent, and it was absent from the first draft of this
  section too, which claimed stories 27–33 whole — the review caught it. There is no
  break-glass permission, route, action or column anywhere in the change. It is a real feature
  with real design questions (who may invoke it, over what, for how long, who is told at the
  time rather than afterwards) and none of them were answered here. What exists that it would
  build on: the audit trail records at a severity, API tokens already carry an expiry, and the
  role table already separates the two administrative roles.
- **Recording a performed remediation (story 17).** The Responder role exists and is a real,
  distinct set of permissions, but there is no route through which a human can state that they
  remediated something. Adding one is the investigation-outcome slice's work: it changes what an
  `OutcomeAssessment` is, and inventing that shape here would have been this spec deciding
  another one's question. The role holds `connection.update` in the meantime, which is the
  remediation this surface actually offers.
- **Contract-drift made non-skippable in CI (story 34).** The test named lives in `oc-frontend`,
  which is a different repository and is out of this repository's reach. What this side owes it
  now exists: a stable, authenticated surface with a session a test can seed.
- **Retention pruning (half of story 21) — BUILT 2026-08-05, and the paragraph it replaces is kept
  because its reasoning is why it was built this way.** It read: the record is immutable and the
  schedule is a column a tenant sets, and the surface reports `auditRetentionEnforced: false`
  rather than implying the schedule is applied; stating a retention period the product does not
  enforce is worse than stating none; the mechanism a pruner will use exists and the pruner does
  not.

  The pruner now exists and the surface reports `true`. Four things about it are decisions rather
  than details:

  - **The declaration is LOCAL to the pruning transaction.** A session-level setting would survive
    on a pooled connection and turn every later transaction that happened to get it into one
    permitted to delete the record, which would make an append-only guarantee depend on connection
    assignment. A test asserts that an ordinary delete is refused, that a declared one succeeds,
    and that ordinary deletes on the same pool are refused again afterwards.
  - **A tenant declaring ZERO is skipped rather than pruned to now.** Zero is the product's default
    of keeping everything, and treating it as a horizon would delete a whole record because
    somebody never set a policy.
  - **Deletes are bounded per statement and per sweep.** A first schedule declared against years of
    history is applied over several sweeps, because one unbounded delete is a lock held while the
    writes queuing behind it are audit events.
  - **The pruner writes no audit event of its own.** An append-only table that gained a row every
    time it was pruned would grow under the mechanism meant to bound it, and the row would say
    nothing an operator could act on. What it produces is a log line per tenant it removed
    anything for.

  The bound worth stating: pruning is against the clock, so a tenant setting thirty days sees rows
  older than thirty days removed on the next hourly sweep rather than at the instant they cross
  the horizon. The surface reports the schedule, and the schedule is what is enforced.

  `auditRetentionEnforced` is passed from the composition root rather than hard-coded true, because
  the statement is made to an auditor and the only way to keep it true is for the component that
  starts the pruner to be the one that says it did.

**SAML 2.0 and SCIM, built in the second pass.**

The first pass deferred them and said why: a hand-rolled SAML implementation is XML signature
verification and canonicalisation, the part of that standard which has produced a decade of
authentication bypasses. That reason is answered rather than overruled — **none of it is
hand-rolled**. `github.com/crewjam/saml` provides the service provider, composing
`goxmldsig` for the signature and `mattermost/xml-roundtrip-validator` for the round-trip check
that refuses the general shape of a signature-wrapping document. Five small modules arrived with
it and no large ones; a gate keeps all four libraries inside `internal/identity`, because a
second package holding them would be a second place a signature could be checked differently.

What this repository wrote is everything around that: which provider, what the request said,
what the assertion means, and what happens next. Specifically —

- **Service-provider initiated only.** IdP-initiated SAML remains out of scope and is now out of
  scope for a mechanical reason rather than a scheduling one: an unsolicited assertion has no
  request of ours to be bound to, so the single-use flow row that makes a replay impossible
  under both protocols would have nothing to consume.
- **The entity identifier and the assertion consumer service are PER PROVIDER**, not one for the
  deployment. The audience restriction the library checks is that value, so an assertion minted
  for one customer's service provider cannot be replayed at another's. A shared entity
  identifier would make every tenant's assertions interchangeable, and the test for it makes an
  otherwise-valid assertion for a different audience and asserts it is refused.
- **AuthnRequests are not signed.** The request carries no secret, the response is bound to it by
  InResponseTo, and the flow row recording it is single-use — so signing would mean holding a
  private key per deployment for a property already held by something simpler. A provider that
  REQUIRES a signed request is not served, and that is a real limit rather than an oversight.
- **A SAML assertion's email is treated as verified.** SAML has no `email_verified` claim and
  does not need one: the assertion is signed by the identity provider the tenant configured,
  asserting an attribute that provider controls. That is a stronger claim than a self-asserted
  OIDC address, and treating it as unverified would mean no SAML tenant could use a verified
  domain policy at all.
- **Everything after a provider is believed is ONE code path for both protocols.** The
  provisioning policy, the verified-domain check, the group mapping, the user resolution and the
  session issue are what a security reviewer asks about, and an answer beginning "for OIDC..."
  would have to be given twice and could drift.

SCIM serves what Okta and Microsoft Entra actually send, and **refuses what it does not
understand rather than ignoring it** — a directory whose deprovisioning silently did nothing
would leave everybody believing access had been removed, which is the worst failure this surface
could have. Three decisions in it are worth stating:

- **A directory decides who is in a group. An ADMINISTRATOR decides what a group grants.** The
  role mapping is on the operator surface behind `identity.configure`, not in SCIM. If a
  directory could decide what its own groups meant here, a change in the customer's identity
  vendor would be a privilege grant in this product, made by whoever can edit a group there. No
  group may map to the owner role at all, for the same reason the just-in-time role and the
  token role may not.
- **An eighth role, `directory_synchroniser`.** The specification proposed seven. A directory's
  credential lives in a customer's identity vendor, and the alternative was issuing it a
  Platform administrator token — a far worse thing to leave in an integration's configuration
  than one narrow role is a departure from a list. It holds `directory.sync` and nothing else,
  exactly as the Auditor holds `audit.read` and nothing else, and a test asserts it reaches no
  other route.
- **`/scim/v2` is not under `/operator/v1`.** An operator API version is this product's to
  change; a directory's base URL is configured once in somebody else's system and then untouched
  for years. Pinning the provisioning surface to the standard's own version means an operator
  API bump is not a change every customer has to make in their identity vendor.

Two things in the second pass were found by its own tests rather than by review, and both are
worth recording because both were design errors rather than typos. `active` and "holds a role"
were the same column, so a person a directory had just created as active read back inactive
until a group was mapped — the directory being told something untrue about its own data; they
are now separate, and a membership may hold no role. And a provisioned person signing in for the
first time would have become a SECOND account, because a directory knows a userName and this
product knows an issuer and a subject; the first sign-in now ADOPTS the placeholder identity,
matched on the address, bounded so that only a row this product created for a directory can be
adopted and never one belonging to a real issuer.

**Departures from the text above, with reasons.**

- **"A new route without a declared permission fails to compile" is not literally true, and the
  three things that ARE true are worth stating precisely.** `authz.Privileged` takes the
  permission as a positional argument, so a route that needs one and omits it does not compile —
  that part holds. But `authz.Public` and `authz.Authenticated` compile by design, because the
  sign-in routes and "who am I" have to exist. What stops one of those being used where a
  privileged route was meant is not the compiler: it is `Table.Validate` at startup, which
  refuses an undeclared permission and a privileged route naming no organization, and three
  gates in `internal/gates` — one holding the public routes to a named list with a reason each,
  one holding the authenticated-only routes likewise, and one refusing a `mux.Handle` anywhere
  under `internal/` outside the four packages recorded as having a listener of their own. That
  last gate reads every package rather than a list of the ones that have routes today, so a new
  surface package cannot be silently unscanned. Story 33's intent holds; the mechanism is a
  build failure at `go test` rather than at `go vet`, and saying "compile error" would overstate
  it.

- **Table names are singular** (`app_user`, `operator_session`, `audit_event`), not plural. Every
  other table in this schema is singular, and following the plural names here would have made
  migration 0011 the odd one out forever. `app_user` rather than `user` because `user` is reserved
  in Postgres; `operator_session` because a relay session already exists in this schema and is a
  different subject.
- **`connection.delete` does not exist.** The spec's story 16 implies deletion of Connections, and
  the product has never had it: a Connection is disabled, so the record of what a source produced
  survives. The permission was written, the gate refused it as reachable by no route, and it was
  removed rather than given a route nobody asked for. An Investigator being unable to damage the
  estate is still true and is asserted.
- **The principal reaches storage on mutations and on the relay reads, not on every read.** The
  spec asks for `(ctx, principal, organization, …)` on every store call site. Every mutating
  operator-facing function takes it and refuses a membership miss with `ErrNotAMember`, and so do
  the relay roster, the conflict trail, and everything new. The investigation READ models keep
  their signatures. The reason is that the middleware already covers every route from a table
  that a gate proves complete, so the marginal value there is small and the cost was forty
  signatures and their call sites; the reason it was done for mutations anyway is that the actor
  has to reach that layer regardless, for the audit row the write commits alongside itself.
- **Ordinary list reads are logged rather than audited.** The spec asks for an audit event on
  "every cross-tenant read". With memberships enforced, no read is cross-tenant any more — a
  caller reads a tenant they belong to. The reads that were previously called out as crossing a
  boundary keep their loud treatment: the relay roster and the conflict trail log, and clearing a
  conflict writes an event at warning level in the transaction that destroys the finding. Auditing
  every list read would put a row in an append-only table on every page load.
- **The bootstrap credential has no expiry and no revocation row.** It is bound to one
  Organization and one role, which is the property the spec asks for and the difference from the
  ambient root token it replaces. It has no deadline because it exists to bootstrap a deployment
  that has no members yet; revoking it means changing the file and restarting. Every token issued
  after that comes from `api_token`, where both exist.
- **ADR-017 is honoured for three of the four new packages and NOT for `internal/identity`.
  This is a known debt, named here rather than discovered later.** `session.Session`,
  `audit.Event` and the `authz` vocabulary all live in the capability that defines them and
  `internal/storage` reconstructs them, which is what the ADR asks. `internal/identity` does the
  opposite: `User`, `IdentityProvider`, `SignInFlow`, `ServiceAccount`, `APIToken` and `Member`
  are declared in `internal/storage` and the capability imports persistence to name its own
  nouns.

  The reason is a real constraint rather than momentum, and it is worth stating because it also
  describes the fix. `internal/identity` holds `*storage.Placements` directly, the way
  `internal/connection` and `internal/environment` do. If storage also owned those types the
  import would run both ways and not compile. The shape that works is the one
  `internal/investigation` already uses — the capability declares a `Store` interface, owns its
  types, and never imports persistence; storage implements the interface and imports the
  capability. Two of the three errors this review moved (`authz.ErrNotAMember`,
  `audit.ErrWriteFailed`) were relocated for exactly that reason, and the compiler is what
  caught it.

  It was not done for the rest in this slice because inverting a thirteen-file package's
  dependency is a refactor with its own review, at the end of a change that is already large and
  green. Doing it badly, unverified, would be worse than recording it. The cost only grows, so
  it should be the first thing the next slice touching identity does.
- **The console must share a registrable domain with the operator surface.** This follows from the
  spec's own choice of `SameSite=Lax` with no CSRF token rather than from a decision made here: a
  cross-site console would never send the cookie at all. It is stated in the README because it is
  a deployment constraint somebody will otherwise discover at integration time.
