# Deployment

One static binary (`CGO_ENABLED=0`), one Postgres per placement, four listeners bound
deliberately. Migrations are embedded and applied at startup under an advisory lock, so
concurrently starting instances cannot race the schema; the log reports what each start
applied.

## The four listeners, and where each belongs

| Listener | Bind it | Because |
| --- | --- | --- |
| Health (`OC_HTTP_ADDRESS`) | where the cluster scrapes | liveness never consults a dependency; readiness reports the placement this instance must have |
| Operator (`OC_OPERATOR_ADDRESS`) | somewhere private, behind TLS | it reads across tenants; empty exposes it nowhere |
| Intake (`OC_INTAKE_ADDRESS`) | publicly, behind a TLS-terminating edge | the one surface a customer's infrastructure reaches inbound; it serves plain HTTP itself, and the webhook secret is a bearer credential — publishing it without TLS hands that credential out in cleartext |
| Relay (`OC_RELAY_ADDRESS`) | publicly, behind the TLS terminator whose SPKI pins the relays hold | relays connect outbound and pin the key, not a CA |

## Lifecycle

- **Startup refuses what cannot work**: an unusable listen address, a placement typo, an
  unparseable App key, an unpriced or unconsented model, a credential-bearing catalog
  with no sealing key. A deployment that starts is one whose configuration can be served.
- **Shutdown drains**: SIGTERM stops accepting, in-flight requests finish inside
  `OC_SHUTDOWN_TIMEOUT`, relay sessions get half the budget to flush, and running
  investigations are failed with the reason recorded rather than orphaned as running.
- **Rolling deploys are safe**: leases are fenced, webhook deliveries are idempotent by
  body digest, and the migration lock serialises schema changes.

## Scaling and tiers

Placement assignments are the tier mechanism: the default placement is the shared tier;
an explicit assignment gives a tenant a dedicated database. Readiness deliberately does
not require every placement — one dedicated tenant's outage must not withdraw the
instance from everyone else.

## Upgrades

Migrations are forward-only and append-only; an applied migration is never edited. The
relay protocol is consumed as a tagged module (`oc-relay/gen/go`), so a control-plane
upgrade never rebuilds relay code; `OC_MINIMUM_RELAY_VERSION` is how the fleet summary
counts stragglers.
