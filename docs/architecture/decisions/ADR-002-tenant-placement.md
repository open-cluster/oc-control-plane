# Tenant placement is resolved, never ambient

OpenCluster sells three isolation tiers — Starter (shared control plane, shared
database), Business (shared control plane, dedicated database), Enterprise (dedicated
control plane deployment, dedicated database) — so where a tenant's data lives must be a
value resolved from the organization, not a single ambient connection string. The
alternative considered and rejected was shared-everything with PostgreSQL row-level
security: it provides provable isolation but answers neither data residency, blast-radius
isolation, per-tenant backup and restore, nor noisy-neighbour control, all of which the
Business and Enterprise tiers exist to sell.

## Consequences

- `IDbConnectionFactory` takes the organization and returns a connection for that
  organization's placement. One implementation returns the shared pool, so no behaviour
  changes on the day it lands.
- The 28 non-test files that construct connections directly become a build failure. An
  architecture test enforces this, in the same shape as the existing dependency-direction
  guards.
- The tier is a configuration value, never a code path. The same binary serves all three
  tiers; only placement resolution and deployment topology differ. CI runs the full suite
  against a second placement to keep that honest.
- Migrations become expand/contract by necessity: with N databases at N versions the
  application must run against schema N and N-1, so a column is added, backfilled,
  switched, and dropped across separate releases. This was optional with one database.
- Evidence snapshot content moves out of PostgreSQL to per-placement object storage. It
  is the sensitive payload and the largest one, so it is also the cheapest residency
  lever.
- The Enterprise tier is gated on deployment automation, not on architecture. A dedicated
  control plane is not sold until the shared one deploys from CI reproducibly with
  automated migrations and a tested rollback.
- Model provider is a placement dimension alongside the database: Starter uses
  OpenCluster's provider, Business and Enterprise may bring their own keys, region, and
  retention terms.
