# Spec — Versioned Integration registry, Connection lifecycle, Relay lifecycle, table query contract

Status: **BUILT 2026-08-05**, commit `127dadf`, migration `0013_connection_lifecycle.sql`. The
operator surface now serves the organization-scoped catalog with lifecycle state, the
Organization-wide Connection list, `validate`, `enabled`, `deliveries`, `trigger/test-event`,
`trigger/rotate-secret`, `dependents`, `validations`, `DELETE`, the Relay fleet summary and
bootstrap tokens. **Not yet verified against the frontend's declared contract** — that is
`oc-frontend/tests/e2e/contract-drift.spec.ts`, which still skips when nothing is listening.

Repo: `oc-control-plane`. Depends on `plans/spec-operator-api-identity-and-rbac.md` for the
principal.
Audit basis: `oc-frontend/plans/audit-2026-08-04-enterprise-forensic.md` §1 B5, B6 and §2 C1–C3.

## Problem Statement

The frontend renders a provider catalog, a Connections list, a trigger-delivery health view and a
Relay fleet from a contract of 31 operations. Sixteen of those are absent from the control plane or
served under a different path with a different shape. The four that carry the most operator value —
fleet summary, Connection validation, trigger delivery history, send test event — do not exist at
all.

Where the catalog is served, it is thin. `GET /operator/v1/integrations`
(`internal/connection/handlers.go:48`) returns `{integration, roles}` and takes no Organization, so
it cannot say how many Connections a tenant already has of a provider — the one fact that turns a
catalog tile from "Add connection" into "Configure". It has no display name, no description, no
category, no capability list, no logo key, no configuration schema, no version and no lifecycle
state.

The vocabulary is two entries wide: `alertmanager` and `kubernetes`
(`internal/connection/integration.go:28-43`). The frontend enum advertises fifteen
(`oc-frontend/entities/contract/onboarding.ts:119-135`). A catalog rendered from the frontend's
list offers thirteen providers a customer cannot connect; `NOTES.md` names the gap in the project's
own words — *"one integration; one cluster type"*.

Meanwhile the frontend hardcodes nothing per provider, which is the good news: `useCatalog`
(`oc-frontend/features/integrations/use-catalog.ts`) reads the catalog from the server and resolves
every provider name through it. The architecture is already dynamic. The registry behind it is not.

For the people involved:

- An **SRE** setting up cannot discover what OpenCluster can reach, or tell a supported provider
  from an aspirational one.
- An **integration manager** cannot configure a second Prometheus, because no Organization-wide
  Connection list or creation path exists.
- An **integration manager** cannot tell a Connection that is configured from one that works,
  because nothing validates.
- An **SRE during an incident** cannot tell a quiet night from a broken intake, because delivery
  health has no source.
- A **platform engineer** with a hundred Relays has no summary, no server-side sort, no cursor
  pagination and no filter the backend honours.

## Solution

Make the Integration registry a versioned, Organization-scoped, backend-owned artifact, and serve
the operations the frontend already declares.

An `IntegrationDefinition` gains everything a schema-driven setup flow needs: stable id and slug,
a definition version, display name, description, category, an approved logo asset key,
documentation link, supported authentication modes, supported connection modes, supported
execution localities, a JSON Schema for its configuration, a presentation schema for how to lay
that out, its typed capabilities, a minimum Relay version, whether multiple instances are
supported, and a **lifecycle state**. Lifecycle state is what lets the catalog be honest: `general`
and `preview` are actionable, `planned` and `deprecated` are labelled and not.

A Connection gains a real lifecycle: create, validate (with partial success), enable, disable,
rotate credentials, revise, delete — each with a configuration revision number and an audit event.
Credentials are written once and never read back; what the browser sees is a credential reference
and its metadata.

The Relay fleet gains its summary, its server-side filters, its cursor pagination, and a
bootstrap-token operation on the operator surface. It does **not** gain an Environment column: a
Relay is Organization-scoped and carries no Environment (`CONTEXT.md`), and the frontend already
implements the correct derived presentation (`oc-frontend/features/relays/relays-view.tsx:30-38`).

One table query contract covers every list endpoint, so the frontend's ResourceTable has one shape
to speak to rather than five.

## User Stories

**Catalog**

1. As an SRE, I want to see which providers OpenCluster can actually reach, so that I can plan a
   deployment against facts rather than a roadmap.
2. As an SRE, I want a provider that is planned but not implemented to be clearly labelled and not
   clickable, so that I do not spend an afternoon configuring something that cannot work.
3. As an SRE, I want to see a provider's official logo beside its name, so that I can find what I
   am looking for by shape at a glance.
4. As an SRE, I want to see how many Connections my Organization already has of a provider, so
   that the tile tells me whether I am adding or extending.
5. As an SRE, I want to see which typed capabilities a provider supplies, so that I know what
   evidence connecting it makes available.
6. As an SRE, I want to see whether a provider runs centrally or through a Relay, so that I know
   whether I need to install anything.
7. As an SRE, I want to search and filter the catalog by category and state, so that a growing list
   stays usable.
8. As an SRE, I want a link to the provider's setup documentation from its catalog entry, so that
   I do not have to search for it.
9. As a developer, I want to add a provider by adding a definition and an adapter, not a React
   page, so that the catalog grows without frontend work.
10. As a developer, I want a definition version, so that a frontend built against an older
    definition can detect that it is stale rather than mis-render it.

**Connections**

11. As an integration manager, I want to configure several Connections of one Integration, so that
    three Prometheus installations are an ordinary estate rather than an unsupported case.
12. As an integration manager, I want to name each Connection myself, so that
    `prometheus-production-eu` is distinguishable from `prometheus-staging` during an incident.
13. As an integration manager, I want to list every Connection in my Organization across
    Environments, so that I can see the estate rather than one Environment at a time.
14. As an integration manager, I want to filter Connections by Environment, provider, role and
    state, so that I can find one in a list of two hundred.
15. As an integration manager, I want the setup form for a provider to come from the backend
    definition, so that a new provider does not wait on a frontend release.
16. As an integration manager, I want provider-specific setup steps where the provider genuinely
    needs them — AWS role assumption, Kubernetes service accounts, OAuth consent — so that a
    generic form engine does not make the hard cases impossible.
17. As an integration manager, I want to validate a Connection before saving it, so that I find out
    it is wrong now rather than during an incident.
18. As an integration manager, I want validation to report partial success, so that "authentication
    worked, two of five capabilities are unavailable" is a result rather than a failure.
19. As an integration manager, I want to see validation history, so that I can tell an outage from
    a configuration that never worked.
20. As an integration manager, I want to submit a credential and never see it again, so that the
    browser is not a place a secret can be read.
21. As an integration manager, I want to see which credential is in use by reference and metadata —
    method, fingerprint, expiry, last rotation — so that I can tell an expired token from a revoked
    one without seeing either.
22. As an integration manager, I want to rotate a credential without recreating the Connection, so
    that rotation does not lose the Connection's identity and history.
23. As an integration manager, I want to disable a Connection without deleting it, so that I can
    stop it participating while an investigation into it is open.
24. As an integration manager, I want a configuration revision number, so that "it changed" is
    answerable.
25. As an integration manager, I want to delete a Connection and be told what depends on it first,
    so that I do not silently break an Environment's coverage.
26. As an auditor, I want every Connection lifecycle operation recorded with the actor, so that the
    estate's history is readable.

**Investigation triggers**

27. As an SRE, I want to see each trigger's delivery endpoint so that I can configure the source
    that posts to it.
28. As an SRE, I want to see when a trigger last received anything and when it last accepted
    something, separately, so that a source delivering and being rejected does not look healthy.
29. As an SRE, I want to see how many deliveries were rejected and why, so that I can fix a
    signature or a clock skew.
30. As an SRE, I want to send a test event, so that I can verify intake without waiting for a real
    incident.
31. As an SRE, I want to see delivery history, so that I can correlate a missed investigation with
    a missed delivery.
32. As an SRE, I want to rotate a trigger's verification secret, so that a leaked webhook URL is
    recoverable.
33. As an SRE, I want a single honest notice when delivery health is unavailable, rather than a
    grid of "Not reported" cells, so that the page does not pad itself with absence.

**Relay fleet**

34. As a platform engineer, I want a fleet summary — total, connected, disconnected, outdated,
    degraded, active requests — so that I can assess a hundred Relays without reading a hundred
    rows.
35. As a platform engineer, I want to search, filter by state, version and capability, and sort,
    with the backend doing the work, so that the page is as fast at a thousand Relays as at ten.
36. As a platform engineer, I want cursor pagination, so that a fleet that grows during pagination
    does not skip or duplicate rows.
37. As a platform engineer, I want to see which Environments a Relay currently serves, derived from
    the Connections bound to it, so that I understand its reach without a field the record does not
    have.
38. As a platform engineer, I want a bootstrap token I can generate for an installation, shown
    once, with a stated expiry, so that installing a Relay does not require sharing a permanent
    secret.
39. As a platform engineer, I want to rotate a Relay's bootstrap token, so that a compromised
    installation credential is recoverable.
40. As a platform engineer, I want each Relay's current and available version, so that I know what
    needs upgrading.
41. As a platform engineer, I want a Relay's recent errors and health history, so that I can
    diagnose an intermittent one.
42. As a platform engineer, I want to see which Connections a Relay serves, so that I know what
    disabling it costs.
43. As a security reviewer, I want a relay identity claimed by two parties to be surfaced to an
    operator, so that the credential-theft detection the control plane already performs is not
    discarded.
44. As a platform administrator, I want clearing a session conflict to require the highest
    privilege and to write a loud audit event, so that destroying a security finding is itself on
    the record.

**Table query contract**

45. As a developer, I want every list endpoint to accept the same query parameters and return the
    same envelope, so that the frontend has one table to build and one contract to test.
46. As an SRE, I want my filters and sort to survive a page reload and a shared URL, so that "look
    at this" in an incident channel is a link.

## Implementation Decisions

**Registry ownership.** The Integration vocabulary stays a closed set compiled into the binary —
`internal/connection/integration.go`'s reasoning is correct and this spec does not reopen it. What
changes is that each entry gains a full definition record rather than a name and a role bitmask,
and the definitions are served Organization-scoped so `configuredConnections` can be counted.

**`IntegrationDefinition`, served shape.** `id`, `slug`, `definitionVersion`, `name`,
`description`, `category`, `logoAssetKey`, `documentationUrl`, `authModes[]`, `connectionModes[]`,
`executionLocalities[]`, `configurationSchema` (JSON Schema draft 2020-12), `presentationSchema`,
`validationContract`, `capabilities[]`, `minimumRelayVersion`, `supportsMultipleInstances`,
`lifecycle`, `configuredConnections`.

`lifecycle` is `general | preview | planned | deprecated`. Today: `kubernetes` and `alertmanager`
are `general`; every other member of the frontend's fifteen-key enum is `planned` until an adapter
exists. **The frontend must render `planned` as non-actionable.** This is the mechanism that makes
the catalog honest instead of aspirational, and it is why the enum does not shrink.

`logoAssetKey` names an asset in the frontend's brand registry; it does not carry an image. The
registry's rules (`oc-frontend/shared/brands/brand-registry.ts:14-24`) stand: no mark is redrawn,
recoloured, cropped or generated, every mark has a manifest entry, and where no approved asset
exists the neutral category icon is used. The backend supplies a key; the frontend decides whether
it has an approved file for it.

**Schema-driven setup with extension slots.** `configurationSchema` plus `presentationSchema`
covers the ordinary case (endpoint, auth method, scope). It will not cover AWS role assumption,
Kubernetes service-account binding or an OAuth consent round-trip, and a form engine stretched to
cover them becomes unusable for the simple cases. So the presentation schema may name a
`providerStep` — an identifier the frontend resolves to a purpose-built component. A provider with
no `providerStep` needs no frontend code; a provider with one needs exactly the component the hard
case requires and nothing more.

**Connection lifecycle.** States: `configured` → `validating` → `active` | `degraded` | `failed`,
plus `disabled` orthogonally. `configurationRevision` increments on every accepted change.

Validation returns a per-capability result rather than a boolean: authentication outcome, then each
declared capability as `available` | `unauthorized` | `unavailable` | `not_attempted` with a
reason. This is what makes story 18 true, and it maps onto the coverage vocabulary
(`CONTEXT.md`: available, degraded, stale, unauthorized, absent) rather than inventing a second
one.

**Credentials.** Written once through an opaque reference to a secret store; no read path returns
one. `internal/connection/secret.go` already establishes this for trigger secrets — *"what is
stored is a digest, no path reads it back, and an operator who loses it rotates rather than
recovers"* — and evidence credentials follow the same rule. What a read returns is
`{ method, reference, fingerprint, createdAt, rotatedAt, expiresAt }`.

**Path corrections.** As listed in `spec-operator-api-identity-and-rbac.md` — organization-scoped
integrations, Organization-wide connections with `environmentId` as a filter, `/enabled` replacing
the `/enable`+`/disable` pair, and the `trigger/` segment on trigger-secret rotation. New
operations: `POST …/connections/{id}/validate`, `GET …/connections/{id}/deliveries`,
`POST …/connections/{id}/trigger/test-event`, `DELETE …/connections/{id}`,
`GET …/relays/summary`, `POST …/relays/bootstrap-tokens`.

**Table query contract.** Every list endpoint accepts `search`, zero or more typed filters,
`sort` (a signed field name), `cursor` and `limit`, and returns
`{ items: [], next: string | null, total: number | null, partial: { field, reason }[] }`.

`total` is nullable because a cursor-paginated query over a large table cannot always answer it
cheaply, and a fabricated count is worse than an absent one. `partial` is how the backend says "I
served this column with no data and here is why" — which is what lets the frontend render one
honest notice instead of a column of "Not reported" (story 33).

`limit` is clamped server-side. `cursor` is opaque and signed; `internal/storage/page.go` already
has the shape and `ErrBadCursor` already exists (`operator.go:291-294`).

**Relay: what this spec refuses to add.** No Environment column, no Environment filter, no Cluster
entity. `CONTEXT.md` is explicit that a Relay carries no Environment and that filtering a fleet by
one is "a filter over a property the record does not have". What ships instead is
`servesEnvironments: string[]`, derived server-side from the Connections bound to the Relay, plus a
`servesEnvironment` **filter over that join** — which answers the operator's real question without
inventing a field. The container digest stays labelled a digest.

**Session conflicts get a surface.** `GET …/relays/{registration}/session-conflicts` and
`POST …/relays/{registration}/clear-conflict` already exist and have no consumer. They become a
section of the Relay detail route. `clear-conflict` requires Platform administrator and writes a
warning-level audit event, because it destroys a credential-theft finding.

## Testing Decisions

A good test asserts the observable contract: given a seeded database and a request, what status,
what body, what audit row. Not which repository method ran.

**Prior art in this repo.** `internal/gates/persisted_enum_test.go` fails the build when a
persisted enum changes shape; `internal/gates/dependency_boundary_test.go` fails on a package
reaching where it should not; `internal/storage/boundary_test.go` covers tenant isolation. All
three patterns extend directly.

**Registry tests.** Every `IntegrationDefinition`'s `configurationSchema` is a valid JSON Schema;
every `presentationSchema` names only fields the configuration schema declares; every
`logoAssetKey` either has an approved asset in the frontend manifest or is absent (not a fabricated
key); every `capabilities` entry names a capability `internal/capability` knows; a definition whose
`lifecycle` is not `general` or `preview` is refused by `CreateConnection`. That last one is the
test that makes the honest catalog structural rather than a copy decision.

**Connection lifecycle tests.** Two Connections of one Integration in one Environment both succeed
(the multiple-instance case). Validation returning partial success is representable and round-trips.
A credential is unreadable after creation through every read path — asserted by iterating the route
table, not by naming routes. Deleting a Connection that an Environment's coverage depends on
reports the dependency.

**Table contract tests.** One table test over every list endpoint: unknown sort field is refused
rather than ignored; a tampered cursor answers `ErrBadCursor`; `limit` above the ceiling is clamped
not refused; results are stable across a concurrent insert.

**Fleet scale tests.** Seed 1, 20, 100 and 1000 Relays and assert the summary matches the list, the
cursor walks every row exactly once, and query time stays bounded. The frontend's fixture control
plane already has a `relays=1000` scenario (`oc-frontend/README.md:32`); the backend needs its
equivalent so the scale claim is tested on both sides.

**Cross-repo drift.** `oc-frontend/tests/e2e/contract-drift.spec.ts` must run in CI against a real
`cmd/controlplane` with a seeded session, and a skip must fail the job. This is the test that would
have caught the sixteen-operation divergence on the day it appeared.

## Out of Scope

Adding adapters for the thirteen `planned` providers — each is its own piece of work with its own
capability contract, and this spec makes their absence honest rather than making them exist.
Customer-authored Integration definitions. A generic form engine intended to cover every possible
provider without extension slots. Cluster as a record (see audit §2 C2). Environment as a Relay
property (see audit §2 C1).

## Further Notes

The correct sequencing is registry before Connections before Relays, because the catalog is what a
setup flow starts from and a Connection cannot be created against a definition that does not exist.

`internal/capability/capability.go` already owns typed capability names and is the right authority
for `IntegrationDefinition.capabilities` — the definitions should reference it rather than carry
their own strings, or the two lists will disagree.

One asymmetry worth deciding deliberately: `shared/brands/brand-registry.ts:40` commits approved
marks for `kubernetes` and `prometheus`, but the adapters that exist are `kubernetes` and
`alertmanager`. Either source a manifest-compliant Alertmanager mark or accept the neutral category
icon for it — but do not leave `prometheus` as the only provider with a logo and no adapter, which
is the current state and reads as a product that is further along than it is.
